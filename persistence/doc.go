// Package persistence provides a bounded bbolt reference for local CRDT
// checkpoints.
//
// A checkpoint saves one complete CRDT snapshot, its frontier and HLC state,
// a durable-transport cursor, and an application-owned opaque outbox in one
// bbolt transaction. It is intended for one process owning one protected
// database file. It is neither a clustered database nor an authenticated
// replication protocol.
//
// Each Store has one concrete StateValidator. The validator runs before a
// checkpoint is committed and whenever one is loaded, so a damaged file or a
// codec/schema mismatch fails closed before a caller restores a replica. The
// caller must still make its local CRDT mutation and its call to Save part of
// its own failure policy, and must not retire tombstones merely because a
// checkpoint exists.
package persistence
