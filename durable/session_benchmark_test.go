package durable

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/counter"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/replica"
	"github.com/coder/websocket"
)

// BenchmarkDurableSameHostFanout compares one writer with 1, 4, and 16 real
// loopback receivers. Every operation waits until each receiver decodes and
// installs the change through replica.Inbox, so it measures fan-out delivery
// rather than enqueue-only throughput. It is a controlled same-host result,
// not a TLS, WAN, or production-capacity claim.
func BenchmarkDurableSameHostFanout(b *testing.B) {
	for _, receiverCount := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("receivers=%d", receiverCount), func(b *testing.B) {
			benchmarkDurableSameHostFanout(b, receiverCount)
		})
	}
}

func benchmarkDurableSameHostFanout(b *testing.B, receiverCount int) {
	b.Helper()
	manifest := durableBenchmarkManifest(b)
	store, err := OpenStore(b.TempDir()+"/relay.db", StoreConfig{
		MaxEvents: uint64(b.N) + 1,
		MaxBytes:  uint64(b.N+1) * 4096,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	handler, group := durableBenchmarkHandler(b, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	receivers := make([]*durableBenchmarkReceiver, 0, receiverCount)
	for index := 0; index < receiverCount; index++ {
		receivers = append(receivers, newDurableBenchmarkReceiver(b, endpoint, manifest, index))
	}
	defer func() {
		for _, receiver := range receivers {
			receiver.close()
		}
	}()
	publisher, closePublisher := newDurableBenchmarkPublisher(b, endpoint, manifest)
	defer closePublisher()
	deadline := time.Now().Add(3 * time.Second)
	for {
		group.mu.Lock()
		connected := len(group.peers) == receiverCount+1
		group.mu.Unlock()
		if connected {
			break
		}
		if time.Now().After(deadline) {
			b.Fatalf("connected peers did not reach %d", receiverCount+1)
		}
		time.Sleep(5 * time.Millisecond)
	}

	writer, err := counter.NewGCounter("writer")
	if err != nil {
		b.Fatal(err)
	}
	changes := make([]replica.Change, b.N)
	for index := range changes {
		delta, err := writer.Increment(1)
		if err != nil {
			b.Fatal(err)
		}
		changes[index], err = durableCounterChange(manifest, "writer", uint64(index+1), delta)
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for index, change := range changes {
		encoded, err := marshalChange(change)
		if err != nil {
			b.Fatal(err)
		}
		if err := publisher.Write(context.Background(), websocket.MessageBinary, encoded); err != nil {
			b.Fatal(err)
		}
		for _, receiver := range receivers {
			sequence, ok := <-receiver.events
			if !ok || sequence != uint64(index+1) {
				b.Fatalf("receiver sequence=%d open=%v want=%d", sequence, ok, index+1)
			}
		}
	}
}

type durableBenchmarkReceiver struct {
	connection *websocket.Conn
	events     chan uint64
	done       chan struct{}
}

func newDurableBenchmarkReceiver(b *testing.B, endpoint string, manifest replica.Manifest, index int) *durableBenchmarkReceiver {
	b.Helper()
	connection := durableBenchmarkDial(b, endpoint, manifest, true)
	state, err := counter.NewGCounter(fmt.Sprintf("receiver-%d", index))
	if err != nil {
		b.Fatal(err)
	}
	inbox, err := replica.NewInbox(manifest, replica.Frontier{}, 128, 1<<20, func(data []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
		if err != nil {
			return err
		}
		return state.ApplyDelta(delta)
	})
	if err != nil {
		b.Fatal(err)
	}
	receiver := &durableBenchmarkReceiver{
		connection: connection,
		events:     make(chan uint64, 1),
		done:       make(chan struct{}),
	}
	go func() {
		defer close(receiver.done)
		defer close(receiver.events)
		for {
			messageType, data, err := connection.Read(context.Background())
			if err != nil {
				return
			}
			if messageType != websocket.MessageBinary {
				return
			}
			sequence, dot, delta, err := unmarshalEvent(data, 1<<20, 128)
			if err != nil {
				return
			}
			event, err := newEventFromWire(manifest, crdt.ProtocolPolicy{}, sequence, dot, delta)
			if err != nil {
				return
			}
			if _, err := inbox.Receive(event.Change); err != nil {
				return
			}
			receiver.events <- event.Sequence
		}
	}()
	return receiver
}

func (receiver *durableBenchmarkReceiver) close() {
	if receiver == nil || receiver.connection == nil {
		return
	}
	_ = receiver.connection.CloseNow()
	<-receiver.done
}

func newDurableBenchmarkPublisher(b *testing.B, endpoint string, manifest replica.Manifest) (*websocket.Conn, func()) {
	b.Helper()
	connection := durableBenchmarkDial(b, endpoint, manifest, false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := connection.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	return connection, func() {
		_ = connection.CloseNow()
		<-done
	}
}

func durableBenchmarkDial(b *testing.B, endpoint string, manifest replica.Manifest, stateVector bool) *websocket.Conn {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	protocols := []string{Subprotocol}
	if stateVector {
		protocols = []string{StateVectorSubprotocol}
	}
	connection, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer benchmark"}},
		Subprotocols: protocols,
	})
	if err != nil {
		b.Fatal(err)
	}
	var hello []byte
	if stateVector {
		hello, err = marshalStateVectorHello(manifest, replica.Frontier{}, defaultStateVectorEntries, 128)
	} else {
		hello, err = marshalHello(manifest, 0)
	}
	if err != nil {
		b.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, hello); err != nil {
		b.Fatal(err)
	}
	messageType, data, err := connection.Read(ctx)
	if err != nil || messageType != websocket.MessageText {
		b.Fatalf("read welcome type=%v data=%q err=%v", messageType, data, err)
	}
	if stateVector {
		remote, _, err := unmarshalStateVectorWelcome(data)
		if err != nil || manifest.Compatible(remote) != nil {
			b.Fatalf("state-vector welcome=%q err=%v", data, err)
		}
		messageType, data, err = connection.Read(ctx)
		if err != nil || messageType != websocket.MessageText {
			b.Fatalf("read catch-up complete type=%v data=%q err=%v", messageType, data, err)
		}
		if _, err := unmarshalCatchUpComplete(data); err != nil {
			b.Fatalf("catch-up complete=%q err=%v", data, err)
		}
	} else if remote, _, err := unmarshalWelcome(data); err != nil || manifest.Compatible(remote) != nil {
		b.Fatalf("welcome=%q err=%v", data, err)
	}
	return connection
}
