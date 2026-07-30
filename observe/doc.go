// Package observe connects a CRDT to an application-owned reactive view.
//
// A Store serializes mutations made through it and publishes an immutable
// application projection after a successful mutation. It is deliberately not
// a CRDT protocol, operation log, persistence layer, or transport. In
// particular, its Version values are process-local UI revisions and must not
// be sent to a peer or used as a replication acknowledgement.
//
// The projection passed to New must be safe to retain after the function
// returns. For maps, slices, pointers, and byte slices that normally means
// returning an owned copy. Store shares one projection with all subscribers;
// subscribers must treat Event.Value as immutable.
//
// Store invokes callbacks asynchronously and never while it holds the Store
// lock. A slow subscriber retains only its newest undelivered event. This
// bounds memory and lets a UI render the latest state, but it means observers
// must use Event.Coalesced and Version gaps when every intermediate mutation
// is significant. Durable replication belongs in replica and durable, not in
// this package.
package observe
