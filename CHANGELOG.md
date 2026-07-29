# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Added disabled-by-default, manifest-bound WebSocket and HTTP/SSE live relay
  reference surfaces in `extensions`, plus runnable provider, race, fuzz,
  duplicate/reorder, concurrency, and loopback benchmark coverage.

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
