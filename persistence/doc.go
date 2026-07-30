// Package persistence provides bounded local Store references for CRDT
// checkpoints.
//
// A checkpoint saves one complete CRDT snapshot, its frontier and HLC state,
// a durable-transport cursor, and an application-owned opaque outbox in one
// local durability boundary. BoltStore uses one bbolt transaction; FileStore
// uses a private file replacement. Both are intended for one process owning a
// protected local path. They are neither clustered databases nor authenticated
// replication protocols.
//
// Each Store has one concrete StateValidator. The validator runs before a
// checkpoint is committed and whenever one is loaded, so a damaged file or a
// codec/schema mismatch fails closed before a caller restores a replica. The
// caller must still make its local CRDT mutation and its call to Save part of
// its own failure policy, and must not retire tombstones merely because a
// checkpoint exists.
package persistence
