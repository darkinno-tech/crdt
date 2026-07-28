// Package crdt provides the shared contracts and protocol capability discovery
// used by this module's state-based CRDT implementations.
//
// Applications normally use a concrete data type from a subpackage, such as
// counter for G-Counters and PN-Counters, set for add-wins OR-Sets, or clock
// for hybrid logical clocks. This root package contains the common CRDT,
// delta, snapshot, and mutation-tag contracts those implementations share.
//
// The framed protocol table is intentionally closed. Use ProtocolPolicy during
// authenticated connection setup to advertise only the state and delta frame
// types a replication group has agreed to exchange. Experimental protocols
// require explicit opt-in and may change before stable promotion.
//
// For installation, examples, and package-level guidance, see the module
// README at https://github.com/DarkInno/crdt.
package crdt
