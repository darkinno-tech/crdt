package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/im10furry/crdt/awareness"
	"github.com/im10furry/crdt/counter"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/replica"
)

// BenchmarkGroupReceiveInstalledDuplicateParallel measures the bounded relay
// work required to recognize a retry whose Dot is already installed. It uses
// RunParallel so the result includes contention on one replication group's
// admission lock; the duplicate must not be re-broadcast to peers.
func BenchmarkGroupReceiveInstalledDuplicateParallel(b *testing.B) {
	_, group, manifest, _ := newCounterServer(b)
	writer, err := counter.NewGCounter("operator-a")
	if err != nil {
		b.Fatal(err)
	}
	delta, err := writer.Increment(1)
	if err != nil {
		b.Fatal(err)
	}
	change := newCounterChange(b, manifest, "operator-a", 1, delta)
	encoded, err := marshalChange(change)
	if err != nil {
		b.Fatal(err)
	}
	authorize := func(peer Peer, _ replica.Manifest, dot replica.Dot) error {
		if peer.ID != dot.Actor {
			return ErrUnauthorized
		}
		return nil
	}
	if err := group.receive(Peer{ID: "operator-a"}, authorize, encoded, 1<<20, 128); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			if err := group.receive(Peer{ID: "operator-a"}, authorize, encoded, 1<<20, 128); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkProviderEndToEndRelayFanout measures one fully admitted G-Counter
// change from a publisher, through the WebSocket handler, to every receiving
// peer. Setup and delta generation are outside the timer; one benchmark
// operation completes only after every observer has decoded and installed the
// corresponding change.
func BenchmarkProviderEndToEndRelayFanout(b *testing.B) {
	for _, receiverCount := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("receivers_%d", receiverCount), func(b *testing.B) {
			server, group, manifest, _ := newCounterServer(b)
			endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
			// Keep exactly one completed delivery per receiver buffered. The
			// benchmark still waits for all receiverCount changes after each
			// Publish, but receiver read loops cannot form a scheduler-dependent
			// cycle while the publisher is waiting for its own relay echo.
			observed := make(chan replica.Change, receiverCount)
			receivers := make([]*Client, 0, receiverCount)
			for receiver := 0; receiver < receiverCount; receiver++ {
				client := benchmarkCounterReceiver(b, endpoint, manifest, fmt.Sprintf("observer-%d", receiver), observed)
				receivers = append(receivers, client)
			}
			publisher, publisherState := benchmarkCounterPublisher(b, endpoint, manifest)
			changes := benchmarkCounterChanges(b, manifest, publisherState, b.N)
			wire, err := marshalChange(changes[0])
			if err != nil {
				b.Fatal(err)
			}

			b.SetBytes(int64(len(wire)))
			b.ReportAllocs()
			b.ResetTimer()
			for _, change := range changes {
				if err := publisher.Publish(context.Background(), change); err != nil {
					b.Fatal(err)
				}
				for receiver := 0; receiver < receiverCount; receiver++ {
					if received := <-observed; received.Dot != change.Dot {
						b.Fatalf("received dot = %#v, want %#v", received.Dot, change.Dot)
					}
				}
			}
			b.StopTimer()
			if got := group.Frontier().Counter("publisher"); got != uint64(b.N) {
				b.Fatalf("relay frontier = %d, want %d", got, b.N)
			}
			for _, receiver := range receivers {
				if err := receiver.Close(); err != nil {
					b.Fatal(err)
				}
			}
			if err := publisher.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkProviderEndToEndRelayBatchFanout measures a v2 batch of eight
// independently identified changes. Observers still decode and install every
// Dot, so the benchmark isolates message and fan-out amortization rather than
// changing CRDT delivery semantics.
func BenchmarkProviderEndToEndRelayBatchFanout(b *testing.B) {
	const batchSize = 8
	for _, receiverCount := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("receivers_%d", receiverCount), func(b *testing.B) {
			server, group, manifest, _ := newCounterServer(b)
			endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
			observed := make(chan replica.Change, receiverCount*batchSize)
			receivers := make([]*Client, 0, receiverCount)
			for receiver := 0; receiver < receiverCount; receiver++ {
				client := benchmarkCounterReceiverWithBatches(b, endpoint, manifest, fmt.Sprintf("batch-observer-%d", receiver), observed, true)
				receivers = append(receivers, client)
			}
			publisher, publisherState := benchmarkCounterPublisherWithBatches(b, endpoint, manifest, true)
			changes := benchmarkCounterChanges(b, manifest, publisherState, b.N*batchSize)
			wire, err := marshalChangeBatch(changes[:batchSize])
			if err != nil {
				b.Fatal(err)
			}

			b.SetBytes(int64(len(wire)))
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < len(changes); index += batchSize {
				batch := changes[index : index+batchSize]
				if err := publisher.PublishBatch(context.Background(), batch); err != nil {
					b.Fatal(err)
				}
				remaining := make(map[replica.Dot]int, len(batch))
				for _, change := range batch {
					remaining[change.Dot] = receiverCount
				}
				for receivedCount := 0; receivedCount < receiverCount*len(batch); receivedCount++ {
					received := <-observed
					if remaining[received.Dot] == 0 {
						b.Fatalf("unexpected or duplicate dot %#v", received.Dot)
					}
					remaining[received.Dot]--
				}
			}
			b.StopTimer()
			if got, want := group.Frontier().Counter("publisher"), uint64(len(changes)); got != want {
				b.Fatalf("relay frontier = %d, want %d", got, want)
			}
			for _, receiver := range receivers {
				if err := receiver.Close(); err != nil {
					b.Fatal(err)
				}
			}
			if err := publisher.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

// BenchmarkProviderAwarenessFanout measures a complete v3 awareness heartbeat
// from one authenticated actor through the relay to each observer. It does not
// claim durable delivery: each operation waits only until every observer has
// decoded and installed the current transient state.
func BenchmarkProviderAwarenessFanout(b *testing.B) {
	for _, receiverCount := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("receivers_%d", receiverCount), func(b *testing.B) {
			server, _, manifest := newCounterAwarenessServer(b)
			endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
			observed := make(chan awareness.Update, receiverCount)
			receivers := make([]*Client, 0, receiverCount)
			for receiver := 0; receiver < receiverCount; receiver++ {
				receivers = append(receivers, benchmarkAwarenessClient(b, endpoint, manifest, fmt.Sprintf("presence-observer-%d", receiver), nil, observed))
			}
			publisherStore := mustAwarenessStore(b)
			publisher := benchmarkAwarenessClient(b, endpoint, manifest, "presence-publisher", publisherStore, nil)
			updates := make([]awareness.Update, b.N)
			for index := range updates {
				update, err := publisherStore.Set("presence-publisher", []byte(`{"cursor":{"anchor":"publisher:2048","association":"before"},"name":"Publisher"}`), time.Now())
				if err != nil {
					b.Fatal(err)
				}
				updates[index] = update
			}
			wire, err := marshalAwareness(updates[0], publisherStore.Options())
			if err != nil {
				b.Fatal(err)
			}

			b.SetBytes(int64(len(wire)))
			b.ReportAllocs()
			b.ResetTimer()
			for _, update := range updates {
				if err := publisher.PublishAwareness(context.Background(), update); err != nil {
					b.Fatal(err)
				}
				for receiver := 0; receiver < receiverCount; receiver++ {
					if received := <-observed; received.Actor != update.Actor || received.Clock != update.Clock {
						b.Fatalf("received awareness = %#v, want %#v", received, update)
					}
				}
			}
			b.StopTimer()
			for _, receiver := range receivers {
				if err := receiver.Close(); err != nil {
					b.Fatal(err)
				}
			}
			if err := publisher.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}

func benchmarkCounterPublisher(b testing.TB, endpoint string, manifest replica.Manifest) (*Client, *counter.GCounter) {
	return benchmarkCounterPublisherWithBatches(b, endpoint, manifest, false)
}

func benchmarkAwarenessClient(b testing.TB, endpoint string, manifest replica.Manifest, actor string, store *awareness.Store, observed chan<- awareness.Update) *Client {
	b.Helper()
	if store == nil {
		store = mustAwarenessStore(b)
	}
	client, err := Dial(context.Background(), endpoint, manifest, ClientConfig{
		Header:          http.Header{"Authorization": []string{"Bearer " + actor}},
		EnableAwareness: true,
		OnChange:        func(replica.Change) error { return nil },
		OnAwareness: func(update awareness.Update) error {
			if _, err := store.Apply(update, time.Now()); err != nil {
				return err
			}
			if observed != nil {
				observed <- update
			}
			return nil
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	return client
}

func benchmarkCounterPublisherWithBatches(b testing.TB, endpoint string, manifest replica.Manifest, enableBatches bool) (*Client, *counter.GCounter) {
	b.Helper()
	state, err := counter.NewGCounter("publisher")
	if err != nil {
		b.Fatal(err)
	}
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		b.Fatal(err)
	}
	inbox, err := replica.NewInbox(manifest, frontier, 16, 32<<10, func(data []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
		if err != nil {
			return err
		}
		return state.ApplyDelta(delta)
	})
	if err != nil {
		b.Fatal(err)
	}
	client, err := Dial(context.Background(), endpoint, manifest, ClientConfig{
		Header:        http.Header{"Authorization": []string{"Bearer publisher"}},
		EnableBatches: enableBatches,
		OnChange: func(change replica.Change) error {
			_, err := inbox.Receive(change)
			return err
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	return client, state
}

func benchmarkCounterReceiver(b testing.TB, endpoint string, manifest replica.Manifest, actor string, observed chan<- replica.Change) *Client {
	return benchmarkCounterReceiverWithBatches(b, endpoint, manifest, actor, observed, false)
}

func benchmarkCounterReceiverWithBatches(b testing.TB, endpoint string, manifest replica.Manifest, actor string, observed chan<- replica.Change, enableBatches bool) *Client {
	b.Helper()
	state, err := counter.NewGCounter(actor)
	if err != nil {
		b.Fatal(err)
	}
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		b.Fatal(err)
	}
	inbox, err := replica.NewInbox(manifest, frontier, 16, 32<<10, func(data []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
		if err != nil {
			return err
		}
		return state.ApplyDelta(delta)
	})
	if err != nil {
		b.Fatal(err)
	}
	client, err := Dial(context.Background(), endpoint, manifest, ClientConfig{
		Header:        http.Header{"Authorization": []string{"Bearer " + actor}},
		EnableBatches: enableBatches,
		OnChange: func(change replica.Change) error {
			delivery, err := inbox.Receive(change)
			if err != nil {
				return err
			}
			if delivery.Accepted() {
				observed <- change
			}
			return nil
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	return client
}

func benchmarkCounterChanges(b testing.TB, manifest replica.Manifest, state *counter.GCounter, count int) []replica.Change {
	b.Helper()
	changes := make([]replica.Change, count)
	for index := range changes {
		delta, err := state.Increment(1)
		if err != nil {
			b.Fatal(err)
		}
		changes[index] = newCounterChange(b, manifest, "publisher", uint64(index+1), delta)
	}
	return changes
}
