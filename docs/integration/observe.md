# Application change observation

`observe.Store` is the application-facing reactive boundary for one CRDT. It
serializes mutations that pass through it and publishes a typed, immutable UI
projection after each successful mutation. It is intentionally separate from
CRDT protocol, durable replay, transport, authentication, and persistence.

Use it when a browser, mobile client, desktop view, or service projection must
react to local edits, successfully installed remote deltas, merges, recovery,
or approved maintenance. Do not send `observe.Event.Version` to peers: it is a
process-local UI revision, not a causal clock or durable acknowledgement.

## Distributed counter observers

`NewGCounterObserver` and `NewPNCounterObserver` combine the existing counter
delta APIs with this local observation boundary. A local `Increment` or
`Decrement` returns the canonical counter delta for an authenticated transport;
the receiving side decodes it with the usual counter limits and calls
`ApplyDelta`.

```go
model, err := observe.NewGCounterObserver("browser-tab")
if err != nil { /* handle */ }

subscription, err := model.Subscribe(func(event observe.Event[observe.GCounterView]) {
	if event.Value.Overflow {
		showAggregateOverflow()
		return
	}
	renderCounter(event.Value.Value)
})
if err != nil { /* handle */ }
defer subscription.Unsubscribe()

delta, err := model.Increment(1)
if err != nil { /* handle */ }
encoded, err := delta.MarshalBinary() // authenticate and send outside this object
if err != nil { /* handle */ }

received, err := counter.UnmarshalGCounterDelta(encoded)
if err != nil { /* reject */ }
changed, err := model.ApplyDelta(received)
if err != nil { /* reject */ }
// changed is false for a duplicate/subsumed delivery, which emits no Remote UI revision.
_ = changed
```

`PNCounterView.Value` is a decimal string, so a callback never receives a
mutable `big.Int` or loses valid uint64 component range. These helpers add no
network, authentication, acknowledgement, persistence, or recovery contract;
the host still owns framing admission, durable outbox/state installation, and
the replica/manifest protocol boundary.

## Bind a counter to a view

The view function runs while `Store` serializes an operation. It must return a
value that callers can retain safely. Scalars and strings are naturally safe;
for maps, slices, `[]byte`, or pointers, return an owned copy. Every callback
receives the same projection and must treat it as immutable.

```go
value, err := counter.NewGCounter("browser-tab")
if err != nil { /* handle */ }

model, err := observe.New(value, func(current *counter.GCounter) uint64 {
	// Value is a copied aggregate, not a reference to the counter map.
	total, err := current.Value()
	if err != nil {
		panic(err) // Choose an application-specific invariant policy instead.
	}
	return total
})
if err != nil { /* handle */ }

subscription, err := model.Subscribe(func(event observe.Event[uint64]) {
	// Move this onto the UI framework's dispatcher when it requires a UI thread.
	renderCounter(event.Value)
	if event.Coalesced != 0 {
		metrics.Record("crdt_ui_events_coalesced", event.Coalesced)
	}
})
if err != nil { /* handle */ }
defer subscription.Unsubscribe()

if err := model.Mutate(observe.Local, func(current *counter.GCounter) error {
	_, err := current.Increment(1)
	return err
}); err != nil { /* handle */ }
```

Subscribe atomically queues `Origin == observe.Initial`, closing the usual gap
between reading current state and registering a listener. If a callback is too
slow to start, that initial event can be coalesced into a later event; the
later event still contains the newest complete projection. Use
`SubscribeFromNow` only when the caller has already obtained a coherent
`Snapshot`.

## Apply a remote delta

Run all state-changing paths through the same Store. The CRDT remains
responsible for delta idempotence and conflict semantics; `observe` supplies
the local view revision and notification order.

```go
if err := model.Mutate(observe.Remote, func(current *counter.GCounter) error {
	return current.ApplyDelta(receivedDelta)
}); err != nil {
	// Rejecting an invalid delta publishes no event.
	return err
}
```

Likewise use `observe.Merge`, `observe.Restore`, or `observe.Maintenance` for
the corresponding successful operation. A successful duplicate delta may emit
another `Remote` view event through generic `Mutate`, because it cannot infer
semantic change for every CRDT type. `MutateIf` and the counter observers let a
caller with a type-specific changed result suppress that redundant revision.

## Delivery and lifecycle contract

| Concern | Contract |
| --- | --- |
| Ordering | Each subscriber sees strictly increasing Store versions, except that it can skip superseded revisions. |
| Backpressure | A subscriber has one pending mailbox slot. A slow callback receives the latest state with `Coalesced > 0`; mutation callers never wait for callbacks. |
| Reentrancy | Callbacks execute after Store and CRDT locks are released, so they may invoke a later `Mutate`. A mutation closure itself must not recursively call the same Store. |
| Callback failure | A callback panic is recovered, recorded by `Subscription.Panic`, and only that subscription stops. Optional `Options.OnPanic` is diagnostic. |
| Shutdown | `Unsubscribe` is idempotent. `Done` closes once an in-flight callback exits. `Store.Close` rejects later mutations/subscriptions and cancels all listeners. |
| State ownership | Do not mutate a CRDT directly after placing it in a Store. Use `Snapshot` for an explicit current read and `Mutate` for every update. |

`Coalesced` is suitable for redraw metrics and cache invalidation. It is not a
substitute for a durable operation stream: workflows that must process every
intermediate operation must use authenticated, bounded transport plus the
existing replica/durable recovery contracts.

## Safety and cost boundaries

- `observe` adds no frame types, payloads, network endpoints, goroutine per
  mutation, or persistence. It cannot authenticate a peer or make tombstone
  GC safe.
- Each active subscription owns one dispatcher goroutine and at most one
  pending event; memory is O(subscriptions), not O(mutations).
- Publishing touches each active subscription, so fan-out is O(subscriptions).
  With no subscribers, `Mutate` advances a revision but does not call the view
  function or allocate an event.
- Keep the projection short and copying bounded. Never expose secret CRDT
  payloads to an untrusted UI callback merely because a state change occurred.

## Verification

```sh
go test ./observe -count=1
go test -race ./observe -count=1
GOMAXPROCS=1 go test -run '^$' -bench 'BenchmarkGCounterBinding' -benchmem ./observe
GOMAXPROCS=1 go test -run '^$' -bench 'BenchmarkGCounterObserverRemoteApply' -benchmem ./observe
```

The package tests cover initial delivery, errors, slow-subscriber coalescing,
reentrant callbacks, callback panics, shutdown, concurrent revisions, and a
three-replica G-Counter partition with duplicate/out-of-order remote deltas.
