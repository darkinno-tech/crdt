package crdt_test

import (
	"bytes"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/delta"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/register"
	"github.com/darkinno-tech/crdt/set"
)

const (
	realisticReplicaCount      = 4
	realisticRounds            = 3
	realisticWritesPerReplica  = 64
	realisticFramesPerBatch    = 24
	realisticDeltaMaxBatchSize = 1 << 20
)

type realisticFrame struct {
	bytes []byte
}

type realisticReplicationStats struct {
	deltaFrames uint64
	deltaBytes  uint64
	stateFrames uint64
	stateBytes  uint64
	duration    time.Duration
	totalAlloc  uint64
	heapDelta   int64
	gcCycles    uint32
}

// TestGSetAndMVRegisterRealisticReplication simulates four warehouse sites.
// Each site writes inventory membership and a causally replicated status while
// deliveries are batched, duplicated, reordered, partitioned, and eventually
// repaired from framed snapshots. It exercises the public wire path rather
// than passing in-memory deltas directly between replicas.
func TestGSetAndMVRegisterRealisticReplication(t *testing.T) {
	stats, err := runRealisticGSetMVRegisterReplication()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf(
		"realistic G-Set/MV-Register replication: sites=%d writes/type=%d delta_frames=%d delta_bytes=%d state_frames=%d state_bytes=%d duration=%s total_alloc=%d heap_delta=%d gc_cycles=%d",
		realisticReplicaCount,
		realisticReplicaCount*realisticRounds*realisticWritesPerReplica,
		stats.deltaFrames,
		stats.deltaBytes,
		stats.stateFrames,
		stats.stateBytes,
		stats.duration,
		stats.totalAlloc,
		stats.heapDelta,
		stats.gcCycles,
	)
}

func runRealisticGSetMVRegisterReplication() (realisticReplicationStats, error) {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()

	codec := integrationStringCodec{}
	gsets := make([]*set.GSet[string], realisticReplicaCount)
	registers := make([]*register.MVRegister, realisticReplicaCount)
	for replica := range gsets {
		var err error
		gsets[replica], err = set.NewGSet(fmt.Sprintf("warehouse-%d", replica), codec)
		if err != nil {
			return realisticReplicationStats{}, err
		}
		registers[replica], err = register.NewMVRegister(fmt.Sprintf("warehouse-%d", replica))
		if err != nil {
			return realisticReplicationStats{}, err
		}
	}

	random := rand.New(rand.NewSource(20260729))
	stats := realisticReplicationStats{}
	expectedFinalValues := make(map[string]struct{}, realisticReplicaCount)
	for round := 0; round < realisticRounds; round++ {
		queues := make([][]realisticFrame, realisticReplicaCount)
		for source := range gsets {
			for write := 0; write < realisticWritesPerReplica; write++ {
				element := fmt.Sprintf("inventory/%d/warehouse-%d/item-%03d", round, source, write)
				gsetDelta, err := gsets[source].Add(element)
				if err != nil {
					return realisticReplicationStats{}, err
				}
				gsetFrame, err := gsetDelta.MarshalBinary(codec)
				if err != nil {
					return realisticReplicationStats{}, err
				}
				enqueueRealisticFrame(random, queues, source, round, gsetFrame, &stats)

				value := realisticValue(round, source, write)
				mvDelta, err := registers[source].Set(value)
				if err != nil {
					return realisticReplicationStats{}, err
				}
				mvFrame, err := mvDelta.MarshalBinary()
				if err != nil {
					return realisticReplicationStats{}, err
				}
				enqueueRealisticFrame(random, queues, source, round, mvFrame, &stats)
				if round == realisticRounds-1 && write == realisticWritesPerReplica-1 {
					expectedFinalValues[string(value)] = struct{}{}
				}
			}
		}
		shuffleRealisticQueues(random, queues)
		if err := deliverRealisticBatches(queues, gsets, registers, codec); err != nil {
			return realisticReplicationStats{}, err
		}
	}

	if err := replaceFirstReplicaFromSnapshot(gsets, registers, codec); err != nil {
		return realisticReplicationStats{}, err
	}
	if err := repairRealisticReplicaStates(gsets, registers, codec, &stats); err != nil {
		return realisticReplicationStats{}, err
	}

	if err := verifyRealisticConvergence(gsets, registers, expectedFinalValues); err != nil {
		return realisticReplicationStats{}, err
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	stats.duration = time.Since(started)
	stats.totalAlloc = after.TotalAlloc - before.TotalAlloc
	stats.heapDelta = int64(after.HeapAlloc) - int64(before.HeapAlloc)
	stats.gcCycles = after.NumGC - before.NumGC
	return stats, nil
}

func enqueueRealisticFrame(random *rand.Rand, queues [][]realisticFrame, source, round int, encoded []byte, stats *realisticReplicationStats) {
	for target := range queues {
		if target == source || !realisticSitesConnected(round, source, target) {
			continue
		}
		for duplicate := 0; duplicate < 1+random.Intn(3); duplicate++ {
			queues[target] = append(queues[target], realisticFrame{bytes: encoded})
			stats.deltaFrames++
			stats.deltaBytes += uint64(len(encoded))
		}
	}
}

func realisticSitesConnected(round, left, right int) bool {
	if round != 1 {
		return true
	}
	return (left < realisticReplicaCount/2) == (right < realisticReplicaCount/2)
}

func shuffleRealisticQueues(random *rand.Rand, queues [][]realisticFrame) {
	for _, queue := range queues {
		random.Shuffle(len(queue), func(left, right int) {
			queue[left], queue[right] = queue[right], queue[left]
		})
	}
}

func deliverRealisticBatches(queues [][]realisticFrame, gsets []*set.GSet[string], registers []*register.MVRegister, codec integrationStringCodec) error {
	var group sync.WaitGroup
	errors := make(chan error, len(queues))
	for target, queue := range queues {
		target, queue := target, queue
		group.Add(1)
		go func() {
			defer group.Done()
			for first := 0; first < len(queue); first += realisticFramesPerBatch {
				last := first + realisticFramesPerBatch
				if last > len(queue) {
					last = len(queue)
				}
				items := make([][]byte, last-first)
				for index, packet := range queue[first:last] {
					items[index] = packet.bytes
				}
				batch, err := delta.NewBatch(items, realisticDeltaMaxBatchSize)
				if err != nil {
					errors <- err
					return
				}
				encoded, err := batch.MarshalBinary(realisticDeltaMaxBatchSize)
				if err != nil {
					errors <- err
					return
				}
				received, err := delta.UnmarshalBatch(encoded, realisticFramesPerBatch, realisticDeltaMaxBatchSize)
				if err != nil {
					errors <- err
					return
				}
				for _, item := range received.Items() {
					decoded, err := frame.UnmarshalFrame(item, frame.DefaultLimits())
					if err != nil {
						errors <- err
						return
					}
					switch decoded.TypeID {
					case crdt.TypeIDGSetDelta:
						change, err := set.UnmarshalGSetDelta(item, codec)
						if err == nil {
							err = gsets[target].ApplyDelta(change)
						}
						if err != nil {
							errors <- err
							return
						}
					case crdt.TypeIDMVRegisterDelta:
						change, err := register.UnmarshalMVRegisterDelta(item)
						if err == nil {
							err = registers[target].ApplyDelta(change)
						}
						if err != nil {
							errors <- err
							return
						}
					default:
						errors <- fmt.Errorf("unexpected delta type %d", decoded.TypeID)
						return
					}
				}
			}
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		return err
	}
	return nil
}

func replaceFirstReplicaFromSnapshot(gsets []*set.GSet[string], registers []*register.MVRegister, codec integrationStringCodec) error {
	gsetSnapshot, err := gsets[0].Snapshot()
	if err != nil {
		return err
	}
	recoveredGSet, err := set.NewGSetFromSnapshot("warehouse-0", gsetSnapshot, codec)
	if err != nil {
		return err
	}
	mvSnapshot, err := registers[0].Snapshot()
	if err != nil {
		return err
	}
	recoveredRegister, err := register.NewMVRegisterFromSnapshot("warehouse-0", mvSnapshot)
	if err != nil {
		return err
	}
	gsets[0], registers[0] = recoveredGSet, recoveredRegister
	return nil
}

func repairRealisticReplicaStates(gsets []*set.GSet[string], registers []*register.MVRegister, codec integrationStringCodec, stats *realisticReplicationStats) error {
	gsetStates := make([][]byte, len(gsets))
	mvStates := make([][]byte, len(registers))
	for source := range gsets {
		var err error
		gsetStates[source], err = gsets[source].MarshalBinary()
		if err != nil {
			return err
		}
		mvStates[source], err = registers[source].MarshalBinary()
		if err != nil {
			return err
		}
	}

	var group sync.WaitGroup
	errors := make(chan error, len(gsets))
	for target := range gsets {
		target := target
		group.Add(1)
		go func() {
			defer group.Done()
			for source := range gsets {
				if source == target {
					continue
				}
				wireGSet, err := set.NewGSet(fmt.Sprintf("wire-gset-%d-%d", source, target), codec)
				if err == nil {
					err = wireGSet.UnmarshalBinary(gsetStates[source])
				}
				if err == nil {
					err = gsets[target].Merge(wireGSet)
				}
				if err != nil {
					errors <- err
					return
				}

				wireRegister, err := register.NewMVRegister(fmt.Sprintf("wire-register-%d-%d", source, target))
				if err == nil {
					err = wireRegister.UnmarshalBinary(mvStates[source])
				}
				if err == nil {
					err = registers[target].Merge(wireRegister)
				}
				if err != nil {
					errors <- err
					return
				}
			}
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		return err
	}

	for source := range gsets {
		for target := range gsets {
			if source == target {
				continue
			}
			stats.stateFrames += 2
			stats.stateBytes += uint64(len(gsetStates[source]) + len(mvStates[source]))
		}
	}
	return nil
}

func verifyRealisticConvergence(gsets []*set.GSet[string], registers []*register.MVRegister, expectedFinalValues map[string]struct{}) error {
	wantElements := realisticReplicaCount * realisticRounds * realisticWritesPerReplica
	var canonicalGSet, canonicalRegister []byte
	for replica := range gsets {
		gsetState, err := gsets[replica].MarshalBinary()
		if err != nil {
			return err
		}
		registerState, err := registers[replica].MarshalBinary()
		if err != nil {
			return err
		}
		if replica == 0 {
			canonicalGSet, canonicalRegister = gsetState, registerState
		} else if !bytes.Equal(canonicalGSet, gsetState) || !bytes.Equal(canonicalRegister, registerState) {
			return fmt.Errorf("replica %d did not converge after state repair", replica)
		}
		if got := len(gsets[replica].Elements()); got != wantElements {
			return fmt.Errorf("replica %d has %d G-Set elements, want %d", replica, got, wantElements)
		}
		values := registers[replica].Values()
		if len(values) != len(expectedFinalValues) {
			return fmt.Errorf("replica %d has %d visible MV values, want %d", replica, len(values), len(expectedFinalValues))
		}
		for _, value := range values {
			if _, ok := expectedFinalValues[string(value.Value)]; !ok {
				return fmt.Errorf("replica %d retained unexpected MV value %q", replica, value.Value[:32])
			}
		}
	}
	return nil
}

func realisticValue(round, replica, write int) []byte {
	value := make([]byte, 256)
	label := fmt.Sprintf("status/%d/warehouse-%d/update-%03d", round, replica, write)
	copy(value, label)
	for index := len(label); index < len(value); index++ {
		value[index] = byte('a' + (round+replica+write+index)%26)
	}
	return value
}
