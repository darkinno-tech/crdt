// Package extensions provides opt-in, bounded live transport adapters for CRDT
// replication groups.
//
// Its zero-value feature set exposes no endpoint and never starts an HTTP
// listener. Applications remain responsible for TLS, identity lifecycle,
// durable state and outbox transactions, bootstrap/anti-entropy, membership,
// rate limits, and operations. The adapters are deliberately not a production
// replication service.
package extensions
