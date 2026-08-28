// Package webrtc bridges an already-negotiated reliable WebRTC DataChannel to
// the durable change envelope. It intentionally does not provide signaling,
// identity, TURN credentials, persistence, replay, or durable receipts.
package webrtc

import (
	"errors"
	"fmt"
	"sync"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/durable"
	"github.com/darkinno-tech/crdt/replica"
)

var (
	ErrInvalidConfig = errors.New("crdt WebRTC provider: invalid configuration")
	ErrClosed        = errors.New("crdt WebRTC provider: closed")
	ErrQueueFull     = errors.New("crdt WebRTC provider: inbound queue full")
)

// DataChannel is the small common surface implemented by adapters around a
// browser RTCDataChannel or a Go WebRTC library such as Pion. Applications
// create the peer connection and authenticate/bind signaling before New.
type DataChannel interface {
	Send([]byte) error
	OnMessage(func([]byte))
	OnClose(func())
	OnError(func(error))
	Close() error
}

// Config binds one DataChannel to exactly one manifest. MaxMessageBytes must
// match the negotiated transport body limit; MaxQueued* bounds callback-driven
// inbound work before the application merge callback runs.
type Config struct {
	Channel           DataChannel
	Manifest          replica.Manifest
	Policy            crdt.ProtocolPolicy
	MaxMessageBytes   int
	MaxActorBytes     int
	MaxQueuedMessages int
	MaxQueuedBytes    int
	OnChange          func(replica.Change) error
}

// Provider owns one volatile DataChannel bridge. Send completion means only
// that the selected channel accepted bytes; it is never a durable receipt.
type Provider struct {
	channel  DataChannel
	manifest replica.Manifest
	policy   crdt.ProtocolPolicy
	maxBytes int
	maxActor int
	onChange func(replica.Change) error

	queue chan []byte
	done  chan struct{}

	mu          sync.Mutex
	queuedBytes int
	err         error
	closeOnce   sync.Once
}

// New validates the immutable manifest, installs callbacks, and starts a
// bounded worker. The caller must use an ordered, reliable DataChannel for
// CRDT operation traffic; unordered/lossy channels suit only separate
// presence/awareness data.
func New(config Config) (*Provider, error) {
	if config.Channel == nil || config.OnChange == nil || config.MaxMessageBytes <= 0 || config.MaxActorBytes <= 0 || config.MaxQueuedMessages <= 0 || config.MaxQueuedBytes < config.MaxMessageBytes {
		return nil, ErrInvalidConfig
	}
	if _, err := replica.NewSessionWithPolicy(config.Manifest, config.Policy); err != nil {
		return nil, fmt.Errorf("validate WebRTC manifest: %w", err)
	}
	provider := &Provider{
		channel:  config.Channel,
		manifest: config.Manifest,
		policy:   config.Policy,
		maxBytes: config.MaxMessageBytes,
		maxActor: config.MaxActorBytes,
		onChange: config.OnChange,
		queue:    make(chan []byte, config.MaxQueuedMessages),
		done:     make(chan struct{}),
	}
	config.Channel.OnMessage(provider.enqueue)
	config.Channel.OnClose(func() { provider.stop(ErrClosed) })
	config.Channel.OnError(func(err error) {
		if err == nil {
			err = ErrClosed
		}
		provider.stop(err)
	})
	go provider.run()
	return provider, nil
}

// Publish validates a change against the bound manifest before sending the
// canonical envelope. It does not mutate CRDT state or retain an outbox.
func (provider *Provider) Publish(change replica.Change) error {
	if provider == nil {
		return ErrClosed
	}
	provider.mu.Lock()
	terminal := provider.err
	provider.mu.Unlock()
	if terminal != nil {
		return terminal
	}
	if _, err := replica.NewChangeWithPolicy(provider.manifest, change.Dot, change.Delta(), provider.policy); err != nil {
		return fmt.Errorf("validate WebRTC change: %w", err)
	}
	encoded, err := durable.EncodeChange(change)
	if err != nil {
		return err
	}
	if len(encoded) > provider.maxBytes {
		return durable.ErrStoreFull
	}
	provider.mu.Lock()
	if provider.err != nil {
		err := provider.err
		provider.mu.Unlock()
		return err
	}
	if err := provider.channel.Send(encoded); err != nil {
		provider.mu.Unlock()
		provider.stop(err)
		return err
	}
	provider.mu.Unlock()
	return nil
}

// Done closes once the provider cannot make progress. Err returns the first
// terminal error. A protocol failure closes the DataChannel to avoid retaining
// an unbounded stream of invalid input.
func (provider *Provider) Done() <-chan struct{} {
	if provider == nil {
		return nil
	}
	return provider.done
}

func (provider *Provider) Err() error {
	if provider == nil {
		return ErrClosed
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.err
}

func (provider *Provider) Close() error {
	if provider == nil {
		return ErrClosed
	}
	provider.stop(ErrClosed)
	return nil
}

func (provider *Provider) enqueue(data []byte) {
	if provider == nil || len(data) == 0 || len(data) > provider.maxBytes {
		if provider != nil {
			provider.stop(durable.ErrCorruptStore)
		}
		return
	}
	copyData := append([]byte(nil), data...)
	provider.mu.Lock()
	if provider.err != nil {
		provider.mu.Unlock()
		return
	}
	if provider.queuedBytes > provider.maxBytes-len(copyData) {
		provider.mu.Unlock()
		provider.stop(ErrQueueFull)
		return
	}
	select {
	case provider.queue <- copyData:
		provider.queuedBytes += len(copyData)
		provider.mu.Unlock()
	default:
		provider.mu.Unlock()
		provider.stop(ErrQueueFull)
	}
}

func (provider *Provider) run() {
	for {
		select {
		case <-provider.done:
			return
		case encoded := <-provider.queue:
			provider.mu.Lock()
			provider.queuedBytes -= len(encoded)
			provider.mu.Unlock()
			dot, delta, err := durable.DecodeChange(encoded, provider.maxBytes, provider.maxActor)
			if err == nil {
				var change replica.Change
				change, err = replica.NewChangeWithPolicy(provider.manifest, dot, delta, provider.policy)
				if err == nil {
					err = provider.onChange(change)
				}
			}
			if err != nil {
				provider.stop(fmt.Errorf("receive WebRTC change: %w", err))
				return
			}
		}
	}
}

func (provider *Provider) stop(err error) {
	if provider == nil {
		return
	}
	provider.closeOnce.Do(func() {
		provider.mu.Lock()
		provider.err = err
		close(provider.done)
		provider.mu.Unlock()
		// A few channel implementations synchronously invoke OnClose from
		// Close. Run it separately so that callback's stop re-entry observes
		// closeOnce complete instead of deadlocking on it.
		go func() { _ = provider.channel.Close() }()
	})
}
