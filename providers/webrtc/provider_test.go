package webrtc

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/counter"
	"github.com/im10furry/crdt/durable"
	"github.com/im10furry/crdt/replica"
)

func TestProviderDeliversBoundManifestChange(t *testing.T) {
	left, right := linkedChannels()
	manifest := testManifest(t)
	received := make(chan replica.Change, 1)
	receiver := testProvider(t, right, manifest, func(change replica.Change) error {
		received <- change
		return nil
	})
	defer func() { _ = receiver.Close() }()
	sender := testProvider(t, left, manifest, func(replica.Change) error { return nil })
	defer func() { _ = sender.Close() }()
	change := testChange(t, manifest, "alice", 1, 2)
	if err := sender.Publish(change); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got.Dot != change.Dot || string(got.Delta()) != string(change.Delta()) {
			t.Fatalf("received = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive WebRTC change")
	}
}

func TestProviderClosesOnInvalidInboundAndCallbackFailure(t *testing.T) {
	channel := &fakeChannel{}
	provider := testProvider(t, channel, testManifest(t), func(replica.Change) error { return nil })
	channel.deliver(nil)
	awaitDone(t, provider)
	if !errors.Is(provider.Err(), durable.ErrCorruptStore) {
		t.Fatalf("invalid inbound error = %v", provider.Err())
	}

	channel = &fakeChannel{}
	manifest := testManifest(t)
	provider = testProvider(t, channel, manifest, func(replica.Change) error { return errors.New("application failed") })
	encoded, err := durable.EncodeChange(testChange(t, manifest, "alice", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	channel.deliver(encoded)
	awaitDone(t, provider)
	if provider.Err() == nil {
		t.Fatal("callback failure did not stop provider")
	}
}

func TestProviderBoundsQueueAndSend(t *testing.T) {
	channel := &fakeChannel{}
	manifest := testManifest(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	provider, err := New(Config{Channel: channel, Manifest: manifest, MaxMessageBytes: 1024, MaxActorBytes: 128, MaxQueuedMessages: 1, MaxQueuedBytes: 1024, OnChange: func(replica.Change) error {
		entered <- struct{}{}
		<-release
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := durable.EncodeChange(testChange(t, manifest, "alice", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	channel.deliver(encoded)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	channel.deliver(encoded)
	channel.deliver(encoded)
	awaitDone(t, provider)
	close(release)
	if !errors.Is(provider.Err(), ErrQueueFull) {
		t.Fatalf("queue error = %v", provider.Err())
	}

	channel = &fakeChannel{sendErr: errors.New("network")}
	provider = testProvider(t, channel, manifest, func(replica.Change) error { return nil })
	if err := provider.Publish(testChange(t, manifest, "alice", 1, 1)); err == nil {
		t.Fatal("send error accepted")
	}
	awaitDone(t, provider)
}

func TestProviderRejectsInvalidConfigAndLocalBounds(t *testing.T) {
	manifest := testManifest(t)
	for _, config := range []Config{{}, {Channel: &fakeChannel{}, MaxMessageBytes: 1, MaxActorBytes: 1, MaxQueuedMessages: 1, MaxQueuedBytes: 1}, {Channel: &fakeChannel{}, Manifest: manifest, MaxMessageBytes: 1, MaxActorBytes: 1, MaxQueuedMessages: 1, MaxQueuedBytes: 0, OnChange: func(replica.Change) error { return nil }}} {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%+v) = %v", config, err)
		}
	}
	if _, err := New(Config{Channel: &fakeChannel{}, MaxMessageBytes: 1, MaxActorBytes: 1, MaxQueuedMessages: 1, MaxQueuedBytes: 1, OnChange: func(replica.Change) error { return nil }}); err == nil {
		t.Fatal("invalid manifest accepted")
	}
	channel := &fakeChannel{}
	provider, err := New(Config{Channel: channel, Manifest: manifest, MaxMessageBytes: 1, MaxActorBytes: 128, MaxQueuedMessages: 1, MaxQueuedBytes: 1, OnChange: func(replica.Change) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Publish(testChange(t, manifest, "alice", 1, 1)); !errors.Is(err, durable.ErrStoreFull) {
		t.Fatalf("local max bytes = %v", err)
	}
	if err := testProvider(t, &fakeChannel{}, manifest, func(replica.Change) error { return nil }).Publish(replica.Change{}); err == nil {
		t.Fatal("invalid local change accepted")
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if err := provider.Publish(replica.Change{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed publish = %v", err)
	}
	if (*Provider)(nil).Done() != nil || !errors.Is((*Provider)(nil).Err(), ErrClosed) || !errors.Is((*Provider)(nil).Close(), ErrClosed) || !errors.Is((*Provider)(nil).Publish(replica.Change{}), ErrClosed) {
		t.Fatal("nil provider did not fail closed")
	}
}

func TestProviderHandlesChannelErrorAndByteQueueLimit(t *testing.T) {
	channel := &fakeChannel{}
	provider := testProvider(t, channel, testManifest(t), func(replica.Change) error { return nil })
	channel.fail(nil)
	awaitDone(t, provider)
	if !errors.Is(provider.Err(), ErrClosed) {
		t.Fatalf("nil channel failure = %v", provider.Err())
	}

	channel = &fakeChannel{}
	manifest := testManifest(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	provider, err := New(Config{Channel: channel, Manifest: manifest, MaxMessageBytes: 64, MaxActorBytes: 128, MaxQueuedMessages: 8, MaxQueuedBytes: 64, OnChange: func(replica.Change) error {
		entered <- struct{}{}
		<-release
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := durable.EncodeChange(testChange(t, manifest, "alice", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	channel.deliver(encoded)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("byte queue worker did not start")
	}
	channel.deliver(encoded)
	channel.deliver(encoded)
	channel.deliver(encoded)
	awaitDone(t, provider)
	close(release)
	if !errors.Is(provider.Err(), ErrQueueFull) {
		t.Fatalf("byte queue error = %v", provider.Err())
	}
	channel = &fakeChannel{}
	provider = testProvider(t, channel, manifest, func(replica.Change) error { return nil })
	channel.deliver([]byte{1})
	awaitDone(t, provider)
	if provider.Err() == nil {
		t.Fatal("malformed non-empty frame did not stop")
	}
}

func testProvider(t *testing.T, channel *fakeChannel, manifest replica.Manifest, onChange func(replica.Change) error) *Provider {
	t.Helper()
	provider, err := New(Config{Channel: channel, Manifest: manifest, MaxMessageBytes: 1024, MaxActorBytes: 128, MaxQueuedMessages: 4, MaxQueuedBytes: 4096, OnChange: onChange})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func awaitDone(t *testing.T, provider *Provider) {
	t.Helper()
	select {
	case <-provider.Done():
	case <-time.After(time.Second):
		t.Fatal("provider did not stop")
	}
}

func testManifest(t *testing.T) replica.Manifest {
	t.Helper()
	manifest, err := replica.NewManifest("webrtc-counter", "example.com/webrtc-counter/v1", 1, replica.Protocol{StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func testChange(t *testing.T, manifest replica.Manifest, actor string, sequence, amount uint64) replica.Change {
	t.Helper()
	state, err := counter.NewGCounter(actor)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := state.Increment(amount)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	change, err := replica.NewChange(manifest, replica.Dot{Actor: actor, Counter: sequence}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	return change
}

type fakeChannel struct {
	mu        sync.Mutex
	peer      *fakeChannel
	message   func([]byte)
	closed    func()
	failure   func(error)
	sendErr   error
	closeOnce sync.Once
}

func linkedChannels() (*fakeChannel, *fakeChannel) {
	left, right := &fakeChannel{}, &fakeChannel{}
	left.peer, right.peer = right, left
	return left, right
}

func (channel *fakeChannel) Send(data []byte) error {
	channel.mu.Lock()
	err, peer := channel.sendErr, channel.peer
	channel.mu.Unlock()
	if err != nil {
		return err
	}
	if peer != nil {
		peer.deliver(data)
	}
	return nil
}

func (channel *fakeChannel) OnMessage(callback func([]byte)) {
	channel.mu.Lock()
	channel.message = callback
	channel.mu.Unlock()
}
func (channel *fakeChannel) OnClose(callback func()) {
	channel.mu.Lock()
	channel.closed = callback
	channel.mu.Unlock()
}
func (channel *fakeChannel) OnError(callback func(error)) {
	channel.mu.Lock()
	channel.failure = callback
	channel.mu.Unlock()
}

func (channel *fakeChannel) Close() error {
	channel.closeOnce.Do(func() {
		channel.mu.Lock()
		callback := channel.closed
		channel.mu.Unlock()
		if callback != nil {
			callback()
		}
	})
	return nil
}

func (channel *fakeChannel) deliver(data []byte) {
	channel.mu.Lock()
	callback := channel.message
	channel.mu.Unlock()
	if callback != nil {
		callback(data)
	}
}

func (channel *fakeChannel) fail(err error) {
	channel.mu.Lock()
	callback := channel.failure
	channel.mu.Unlock()
	if callback != nil {
		callback(err)
	}
}
