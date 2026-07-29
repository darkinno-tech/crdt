// Package durable provides a single-writer WebSocket relay reference with a
// persistent operation log, bounded replay, and reconnect support.
//
// It is intentionally separate from extensions: extensions is a bounded live
// relay, while durable owns a bbolt-backed transport log. It remains a
// reference for one active process and one persistent volume, not a clustered
// replication service. Applications must still supply TLS, authentication,
// authorization, concrete CRDT state/frontier checkpoints, durable outboxes,
// membership, and tombstone-GC policy.
package durable
