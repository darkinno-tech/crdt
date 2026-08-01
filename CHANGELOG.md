# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Rejected shared-document local mutations atomically before state or HLC
  changes when their canonical update exceeds the configured output-frame
  budget, preventing local-only writes without an outbox frame.

### Added

- Added disabled-by-default, manifest-bound WebSocket and HTTP/SSE live relay
  reference surfaces in `extensions`, plus runnable provider, race, fuzz,
  duplicate/reorder, concurrency, and loopback benchmark coverage.
- Added the `durable` single-writer WebSocket relay reference: transactional
  bbolt operation retention, exact Dot-to-payload binding, bounded replay,
  reconnect support, restart/partition simulation, fuzzing, and local
  append/replay/reconnect benchmark evidence.
- Added a bounded, manifest-bound WebSocket provider reference and Go client
  example with authenticated upgrade hooks, logical-actor authorization,
  duplicate suppression, and bounded inbox/write queues. It remains a
  reference integration rather than a production synchronization service.
- Added a dependency-free TypeScript decoder for the bounded canonical frame
  envelope and an RGA v1 Go/Wasm client runtime for browser/WebView local
  merges.
- Added real Wasm/Node and browser verification, including three-replica
  duplicate, reordering, malformed-input, and snapshot-recovery scenarios.
- Added RGA snapshot-based delta recovery and compact run-v2 frames for new Go
  replication groups.
- Added bounded retained-state handling for LWW collections and attachment
  references.

### Changed

- Reduced allocations for first large linear RGA inserts and initial syncs by
  batching sequence-pair storage, reusing verified local tag order, and
  pre-sizing empty-document indexes without changing canonical wire bytes.
- Avoided redundant sorting and packed wall-gap allocations when serializing a
  verified linear RGA state, while retaining the existing canonical fallback
  for branching, multi-replica, or unknown-tombstone states.
- Added explicit output and recovery limits for RGA state/delta serialization
  and snapshot restoration, so embedders can use the same budgets at every
  browser-facing boundary.
- Enforced LWW decoder budgets before allocation, required checkpoint-bound
  membership receipts, and classified duplicate replica delivery outcomes.
- Reduced OR-Set merge allocation and improved bounded RGA recovery behavior.

## [1.0.17] - 2026-07-29

### Changed

- Removed machine-specific benchmark host names from the public README.

## [1.0.16] - 2026-07-29

### Added

- Added the cross-host synchronization probe operations runbook and public
  system-context architecture assets.

## [1.0.15] - 2026-07-29

### Changed

- Reorganized public documentation into integration, protocol, design, and
  operations entry points.

## [1.0.14] - 2026-07-29

### Changed

- Removed internal fault-review notes from the public release tree.

## [1.0.13] - 2026-07-29

### Fixed

- Serialized short fuzz workers in CI to avoid unstable concurrent fuzz runs.

## [1.0.12] - 2026-07-29

### Added

- Added the attachment collaboration example and integration guide for
  manifest-bound RGA text and immutable attachment references.

### Fixed

- Verified referenced attachment content by bounded streaming SHA-256 and
  length checks before accepting it.
- Hardened RGA recovery/duplicate delivery and LWW-Map replication recovery.

### Changed

- Added framed LWW-Set replication, bounded/compactable OR-Tree tombstone
  handling, and canonical visible-tree projection caching.

## [1.0.11] - 2026-07-29

### Changed

- This release tag points to the same source revision as `v1.0.9`; it contains
  no additional source changes.

## [1.0.10] - 2026-07-29

### Added

- Added bounded immutable attachment references and content-verification
  guidance.
- Added framed LWW-Set replication with canonical state/delta encoding and
  snapshot recovery.
- Added OR-Tree tombstone limits, leaf-only compaction support, and lifecycle
  documentation.

### Fixed

- Rejected stale experimental replica anchors and hardened LWW-Map and RGA
  recovery paths.

## [1.0.9] - 2026-07-29

### Changed

- Stopped tracking local fault-review artifacts in the public repository.

## [1.0.8] - 2026-07-29

### Added

- Added G-Set and causal MV-Register framed replication, safe JSON diagnostics,
  authority-backed membership epochs, signed gossip reference material, and
  executable replication examples.
- Added RGA bounded out-of-order integration, run-wire coverage, checkpoint
  boundaries, and exact-acknowledgement tombstone-GC support.

### Changed

- Documented the canonical Go module path and public package index; added the
  contribution guide and release changelog.
- Improved delta batching, Merkle root caching, and RGA pending-queue handling.

## [1.0.7] - 2026-07-29

### Changed

- Updated GitHub Actions runtime compatibility and removed the stale
  Staticcheck Node 20 cache dependency.

### Fixed

- Prevented stale release-tag workflow runs from being reused.

## [1.0.6] - 2026-07-29

### Added

- Added framed G-Set and MV-Register replication simulations and RGA
  replication/scale coverage.

### Fixed

- Kept experimental RGA opt-in explicit and linearized pending dependency
  validation before integration.

### Changed

- Improved the RGA resolved-batch fast path and clarified its tombstone
  lifecycle.

## [1.0.5] - 2026-07-29

### Added

- Added G-Set and MV-Register implementations with framed protocol support.
- Added safe, summary-only JSON diagnostics for CRDT states and deltas.

### Fixed

- Synchronized CI fuzz targets and aligned stable-protocol/release guidance.

## [1.0.4] - 2026-07-29

### Fixed

- Normalized the Go module declaration, internal imports, examples, and
  documentation to the canonical `github.com/DarkInno/crdt` path. This removes
  the module-path casing mismatch that prevented reliable `go get` usage.

## [1.0.4-beta.1] - 2026-07-28

### Fixed

- Normalized the Go module declaration, internal imports, examples, and
  documentation to the canonical `github.com/DarkInno/crdt` path. This removes
  the module-path casing mismatch that prevented reliable `go get` usage.

## [1.0.3] - 2026-07-28

### Fixed

- Corrected release-job triggering so stable tags publish their GitHub release
  from the tagged revision.
- Added tag-trigger coverage to the test workflow used by the release job.

## [1.0.2] - 2026-07-28

### Added

- Added a GitHub Actions release workflow that creates stable release notes
  from version tags.
- Recorded the release-publication failure analysis and operating guidance.

## [1.0.1] - 2026-07-28

### Added

- Added convergent collection primitives, LWW and max-register implementations,
  and framed experimental RGA text and OR-Tree collections.
- Added the experimental delta-replicated LWW-Map, including deterministic HLC
  conflict resolution and framed state/delta support.
- Added `ProtocolPolicy` gating so experimental collection frames require
  explicit replication-group opt-in.
- Added end-to-end integration guidance, examples, collection coverage, and
  operational probes.
- Added exact-acknowledgement tombstone collection with membership epochs.

### Changed

- Improved delta application, snapshot marshaling, and frame encoding for
  OR-Set performance.
- Updated CI and static analysis for Go 1.26 compatibility.

## [1.0.0] - 2026-07-28

### Added

- Initial public release of the CRDT core contracts, hybrid logical clock, and
  canonical binary frame encoding.
- State-based G-Counter and PN-Counter implementations, bounded delta batching,
  add-wins OR-Set recovery, snapshots, and Merkle anti-entropy support.
- Safe tombstone collection, convergence/recovery tests, stress benchmarks,
  quality gates, and CRDT synchronization probes.
- English and Chinese project documentation, integration examples, and public
  verification/performance guidance.

### Changed

- Optimized CRC32 reuse and OR-Set duplicate-delta, state-copy, encoding, and
  snapshot paths.
