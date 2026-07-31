//! A bounded native implementation of the stable `rga-run-v2` text protocol.
//!
//! This crate interoperates with DarkInno Go RGA run frames (state TypeID 19,
//! delta TypeID 20). It intentionally does **not** implement the separately
//! negotiated `native-ts-v1` JSON contract. Hosts must authenticate their
//! replication group and persist [`Rga::encode_state`], [`ClockState`], and
//! their own frontier/outbox atomically before reusing a replica ID.

#![forbid(unsafe_op_in_unsafe_fn)]

use std::cmp::Ordering;
use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};
use std::fmt;
use std::sync::OnceLock;
use std::time::{SystemTime, UNIX_EPOCH};

#[cfg(feature = "ffi")]
mod ffi;

/// Stable run-v2 state-frame TypeID.
pub const RGA_RUN_STATE_TYPE_ID: u64 = 19;
/// Stable run-v2 delta-frame TypeID.
pub const RGA_RUN_DELTA_TYPE_ID: u64 = 20;
const FORMAT_VERSION: u64 = 1;
const BLOCK_NODE: u64 = 0;
const BLOCK_CHAIN: u64 = 1;

/// A bounded receiver and retained-state policy.
///
/// These defaults match the browser/Wasm runtime rather than the Go process
/// defaults: a native client should negotiate stricter limits with its group
/// where possible.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Limits {
    /// Maximum complete frame bytes accepted or emitted.
    pub max_frame_bytes: usize,
    /// Maximum decoded RGA payload bytes accepted or emitted.
    pub max_payload_bytes: usize,
    /// Maximum codec and replica identifier bytes.
    pub max_string_bytes: usize,
    /// Maximum nodes in one decoded frame and one retained document.
    pub max_nodes: usize,
    /// Maximum nodes plus tombstones in one decoded frame.
    pub max_tags: usize,
    /// Maximum retained tombstones.
    pub max_tombstones: usize,
    /// Maximum unresolved nodes retained for out-of-order delivery.
    pub max_pending_nodes: usize,
    /// Conservative maximum bytes charged to unresolved nodes.
    pub max_pending_bytes: usize,
}

impl Default for Limits {
    fn default() -> Self {
        Self {
            max_frame_bytes: 1 << 20,
            max_payload_bytes: 1 << 20,
            max_string_bytes: 64 << 10,
            max_nodes: 100_000,
            max_tags: 100_000,
            max_tombstones: 100_000,
            max_pending_nodes: 10_000,
            max_pending_bytes: 512 << 10,
        }
    }
}

impl Limits {
    fn valid(self) -> bool {
        self.max_frame_bytes > 0
            && self.max_payload_bytes > 0
            && self.max_string_bytes > 0
            && self.max_nodes > 0
            && self.max_tags > 0
            && self.max_tombstones > 0
            && self.max_pending_nodes > 0
            && self.max_pending_bytes > 0
    }
}

/// An error from frame decoding, mutation, or resource admission.
#[derive(Clone, Debug, Eq, PartialEq)]
#[non_exhaustive]
pub enum Error {
    /// The frame has a bad magic, checksum, layout, or non-canonical encoding.
    InvalidFrame,
    /// A caller selected invalid limits or a frame/state exceeds them.
    ResourceLimit,
    /// The selected manifest does not permit this TypeID or codec.
    ProtocolMismatch,
    /// The RGA graph, tag, or Unicode scalar is invalid.
    InvalidDelta,
    /// A state snapshot contains an unresolved parent.
    IncompleteState,
    /// One immutable position was received with conflicting content.
    TagConflict,
    /// An edit range lies outside the visible text.
    Range,
    /// The HLC cannot allocate another timestamp.
    ClockExhausted,
}

impl fmt::Display for Error {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::InvalidFrame => "invalid CRDT frame",
            Self::ResourceLimit => "CRDT resource limit exceeded",
            Self::ProtocolMismatch => "CRDT protocol mismatch",
            Self::InvalidDelta => "invalid RGA delta",
            Self::IncompleteState => "RGA state has unresolved parents",
            Self::TagConflict => "conflicting RGA position",
            Self::Range => "visible text range is invalid",
            Self::ClockExhausted => "hybrid logical clock is exhausted",
        })
    }
}

impl std::error::Error for Error {}

/// A stable RGA identifier. Ordering is wall time, logical counter, then raw
/// lexical replica bytes, all ascending.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct Position {
    replica_id: Vec<u8>,
    wall_time: u64,
    logical: u64,
}

impl Position {
    /// Creates a position after validating the protocol's replica-ID rule.
    pub fn new(
        replica_id: impl Into<Vec<u8>>,
        wall_time: u64,
        logical: u64,
    ) -> Result<Self, Error> {
        let value = Self {
            replica_id: replica_id.into(),
            wall_time,
            logical,
        };
        if value.valid() {
            Ok(value)
        } else {
            Err(Error::InvalidDelta)
        }
    }

    /// Returns the raw replica identifier bytes.
    #[must_use]
    pub fn replica_id(&self) -> &[u8] {
        &self.replica_id
    }
    /// Returns the HLC wall-time component in milliseconds.
    #[must_use]
    pub const fn wall_time(&self) -> u64 {
        self.wall_time
    }
    /// Returns the HLC logical component.
    #[must_use]
    pub const fn logical(&self) -> u64 {
        self.logical
    }

    fn valid(&self) -> bool {
        !self.replica_id.is_empty() && !String::from_utf8_lossy(&self.replica_id).trim().is_empty()
    }
}

impl Ord for Position {
    fn cmp(&self, other: &Self) -> Ordering {
        self.wall_time
            .cmp(&other.wall_time)
            .then_with(|| self.logical.cmp(&other.logical))
            .then_with(|| self.replica_id.cmp(&other.replica_id))
    }
}

impl PartialOrd for Position {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

/// Persistable HLC state. Restore it atomically with a complete state frame
/// before using the same logical replica ID again.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ClockState {
    /// Replica identifier used for new local tags.
    pub replica_id: Vec<u8>,
    /// Last witnessed or emitted wall time.
    pub wall_time: u64,
    /// Last witnessed or emitted logical counter.
    pub logical: u64,
}

#[derive(Clone, Debug)]
struct Hlc {
    state: ClockState,
}

impl Hlc {
    fn new(state: ClockState) -> Result<Self, Error> {
        if Position::new(state.replica_id.clone(), state.wall_time, state.logical).is_err() {
            return Err(Error::InvalidDelta);
        }
        Ok(Self { state })
    }

    fn next(&mut self, physical_millis: u64) -> Result<Position, Error> {
        if physical_millis > self.state.wall_time {
            self.state.wall_time = physical_millis;
            self.state.logical = 0;
        } else {
            Self::increment(&mut self.state.wall_time, &mut self.state.logical)?;
        }
        Position::new(
            self.state.replica_id.clone(),
            self.state.wall_time,
            self.state.logical,
        )
    }

    fn witness(&mut self, remote: &Position, physical_millis: u64) -> Result<(), Error> {
        let maximum = self
            .state
            .wall_time
            .max(remote.wall_time)
            .max(physical_millis);
        match (maximum == self.state.wall_time, maximum == remote.wall_time) {
            (true, true) => {
                self.state.logical = self.state.logical.max(remote.logical);
                Self::increment(&mut self.state.wall_time, &mut self.state.logical)?;
            }
            (true, false) => Self::increment(&mut self.state.wall_time, &mut self.state.logical)?,
            (false, true) => {
                self.state.wall_time = remote.wall_time;
                self.state.logical = remote.logical;
                Self::increment(&mut self.state.wall_time, &mut self.state.logical)?;
            }
            (false, false) => {
                self.state.wall_time = maximum;
                self.state.logical = 0;
            }
        }
        Ok(())
    }

    fn increment(wall_time: &mut u64, logical: &mut u64) -> Result<(), Error> {
        if *logical != u64::MAX {
            *logical += 1;
            return Ok(());
        }
        if *wall_time == u64::MAX {
            return Err(Error::ClockExhausted);
        }
        *wall_time += 1;
        *logical = 0;
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct Node {
    parent: Option<Position>,
    value: char,
}

/// A joinable run-v2 delta. It is canonical only after [`Delta::encode`] is
/// called with the selected receiver limits.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Delta {
    nodes: BTreeMap<Position, Node>,
    tombstones: BTreeSet<Position>,
}

impl Delta {
    /// Decodes a bounded, canonical run-v2 delta frame (TypeID 20).
    pub fn decode(bytes: &[u8], limits: Limits) -> Result<Self, Error> {
        let frame = decode_frame(bytes, limits)?;
        if frame.type_id != RGA_RUN_DELTA_TYPE_ID || !frame.codec.is_empty() {
            return Err(Error::ProtocolMismatch);
        }
        decode_payload(&frame.payload, frame.type_id, false, limits)
    }

    /// Encodes the delta as its one canonical TypeID 20 frame.
    pub fn encode(&self, limits: Limits) -> Result<Vec<u8>, Error> {
        encode_payload_frame(
            RGA_RUN_DELTA_TYPE_ID,
            &self.nodes,
            &self.tombstones,
            false,
            limits,
        )
    }

    /// Returns true when this delta carries no nodes or tombstones.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.nodes.is_empty() && self.tombstones.is_empty()
    }
}

/// A bounded, local, mergeable RGA run-v2 document.
#[derive(Clone, Debug)]
pub struct Rga {
    limits: Limits,
    clock: Hlc,
    nodes: BTreeMap<Position, Node>,
    pending: BTreeMap<Position, Node>,
    tombstones: BTreeSet<Position>,
}

impl Rga {
    /// Creates an empty RGA with a fresh clock state.
    pub fn new(replica_id: impl Into<Vec<u8>>, limits: Limits) -> Result<Self, Error> {
        Self::from_clock_state(
            ClockState {
                replica_id: replica_id.into(),
                wall_time: 0,
                logical: 0,
            },
            limits,
        )
    }

    /// Creates an empty RGA after restoring a persisted clock state.
    pub fn from_clock_state(clock: ClockState, limits: Limits) -> Result<Self, Error> {
        if !limits.valid() {
            return Err(Error::ResourceLimit);
        }
        Ok(Self {
            limits,
            clock: Hlc::new(clock)?,
            nodes: BTreeMap::new(),
            pending: BTreeMap::new(),
            tombstones: BTreeSet::new(),
        })
    }

    /// Returns the state that must be persisted with a complete snapshot.
    #[must_use]
    pub fn clock_state(&self) -> ClockState {
        self.clock.state.clone()
    }

    /// Returns visible Unicode-scalar count without exposing tombstoned nodes.
    #[must_use]
    pub fn len(&self) -> usize {
        self.visible_positions().len()
    }

    /// Returns whether no visible nodes are present.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Returns the deterministic visible projection.
    #[must_use]
    pub fn text(&self) -> String {
        self.visible_positions()
            .into_iter()
            .filter_map(|id| self.nodes.get(&id).map(|node| node.value))
            .collect()
    }

    /// Returns unresolved node count. A non-zero count forbids state encoding.
    #[must_use]
    pub fn pending_count(&self) -> usize {
        self.pending.len()
    }

    /// Inserts `value` before a Unicode-scalar offset and returns the exact
    /// canonical frame that has already been applied locally.
    pub fn insert(&mut self, offset: usize, value: &str) -> Result<Vec<u8>, Error> {
        self.insert_at(offset, value, current_millis()?)
    }

    /// Deterministic variant of [`Rga::insert`] for tests and hosts with their
    /// own clock source. `physical_millis` must be a non-negative Unix time.
    pub fn insert_at(
        &mut self,
        offset: usize,
        value: &str,
        physical_millis: u64,
    ) -> Result<Vec<u8>, Error> {
        let visible = self.visible_positions();
        if offset > visible.len() {
            return Err(Error::Range);
        }
        let values: Vec<char> = value.chars().collect();
        if self
            .nodes
            .len()
            .saturating_add(self.pending.len())
            .saturating_add(values.len())
            > self.limits.max_nodes
            || values.len() > self.limits.max_tags
        {
            return Err(Error::ResourceLimit);
        }
        // Build local tags on a private HLC copy. Encoding or admission can
        // still reject this edit, and every rejected operation must retain the
        // caller-visible clock together with its RGA state.
        let mut local_clock = self.clock.clone();
        let mut delta = Delta::default();
        let mut parent = offset
            .checked_sub(1)
            .and_then(|index| visible.get(index))
            .cloned();
        for value in values {
            let id = local_clock.next(physical_millis)?;
            delta.nodes.insert(
                id.clone(),
                Node {
                    parent: parent.clone(),
                    value,
                },
            );
            parent = Some(id);
        }
        let encoded = delta.encode(self.limits)?;
        self.apply_delta_at(&delta, physical_millis)?;
        Ok(encoded)
    }

    /// Tombstones visible Unicode scalars in `[offset, offset + count)` and
    /// returns the exact canonical frame already applied locally.
    pub fn delete(&mut self, offset: usize, count: usize) -> Result<Vec<u8>, Error> {
        self.delete_at(offset, count, current_millis()?)
    }

    /// Deterministic variant of [`Rga::delete`].
    pub fn delete_at(
        &mut self,
        offset: usize,
        count: usize,
        physical_millis: u64,
    ) -> Result<Vec<u8>, Error> {
        let visible = self.visible_positions();
        if offset > visible.len() || count > visible.len() - offset {
            return Err(Error::Range);
        }
        let delta = Delta {
            nodes: BTreeMap::new(),
            tombstones: visible[offset..offset + count].iter().cloned().collect(),
        };
        let encoded = delta.encode(self.limits)?;
        self.apply_delta_at(&delta, physical_millis)?;
        Ok(encoded)
    }

    /// Applies one authenticated, manifest-negotiated frame using the system
    /// clock to witness remote tags.
    pub fn apply_frame(&mut self, bytes: &[u8]) -> Result<(), Error> {
        self.apply_frame_at(bytes, current_millis()?)
    }

    /// Deterministic variant of [`Rga::apply_frame`]. The caller must verify
    /// the replication-group manifest before this method is reached.
    pub fn apply_frame_at(&mut self, bytes: &[u8], physical_millis: u64) -> Result<(), Error> {
        let frame = decode_frame(bytes, self.limits)?;
        if !frame.codec.is_empty() {
            return Err(Error::ProtocolMismatch);
        }
        match frame.type_id {
            RGA_RUN_DELTA_TYPE_ID => self.apply_delta_at(
                &decode_payload(&frame.payload, frame.type_id, false, self.limits)?,
                physical_millis,
            ),
            RGA_RUN_STATE_TYPE_ID => self.install_state_at(
                &decode_payload(&frame.payload, frame.type_id, true, self.limits)?,
                physical_millis,
            ),
            _ => Err(Error::ProtocolMismatch),
        }
    }

    /// Encodes a complete canonical state frame. Pending parent references are
    /// rejected rather than silently persisted as a broken recovery point.
    pub fn encode_state(&self) -> Result<Vec<u8>, Error> {
        if !self.pending.is_empty() {
            return Err(Error::IncompleteState);
        }
        encode_payload_frame(
            RGA_RUN_STATE_TYPE_ID,
            &self.nodes,
            &self.tombstones,
            true,
            self.limits,
        )
    }

    fn install_state_at(&mut self, state: &Delta, physical_millis: u64) -> Result<(), Error> {
        if state.nodes.len() > self.limits.max_nodes
            || state.tombstones.len() > self.limits.max_tombstones
        {
            return Err(Error::ResourceLimit);
        }
        validate_graph(&state.nodes, true)?;
        let mut clock = self.clock.clone();
        if let Some(greatest) = greatest_tag(state) {
            clock.witness(greatest, physical_millis)?;
        }
        self.nodes = state.nodes.clone();
        self.pending.clear();
        self.tombstones = state.tombstones.clone();
        self.clock = clock;
        Ok(())
    }

    fn apply_delta_at(&mut self, delta: &Delta, physical_millis: u64) -> Result<(), Error> {
        validate_delta(delta)?;
        for (id, incoming) in &delta.nodes {
            if let Some(existing) = self.nodes.get(id).or_else(|| self.pending.get(id)) {
                if existing != incoming {
                    return Err(Error::TagConflict);
                }
            }
        }
        if delta
            .nodes
            .iter()
            .all(|(id, node)| self.nodes.get(id).or_else(|| self.pending.get(id)) == Some(node))
            && delta
                .tombstones
                .iter()
                .all(|id| self.tombstones.contains(id))
        {
            return Ok(());
        }

        let mut nodes = self.nodes.clone();
        let mut pending = self.pending.clone();
        let mut tombstones = self.tombstones.clone();
        for (id, node) in &delta.nodes {
            if !nodes.contains_key(id) && !pending.contains_key(id) {
                pending.insert(id.clone(), node.clone());
            }
        }
        validate_graph_with_integrated(&pending, &nodes)?;

        // Index once, then release each reachable descendant from its newly
        // integrated parent. Re-scanning every pending node after every link
        // turns one long locally-generated chain into O(n²) work.
        let mut waiting_by_parent: BTreeMap<Option<Position>, Vec<Position>> = BTreeMap::new();
        let mut ready = Vec::new();
        for (id, node) in &pending {
            waiting_by_parent
                .entry(node.parent.clone())
                .or_default()
                .push(id.clone());
            if node
                .parent
                .as_ref()
                .is_none_or(|parent| nodes.contains_key(parent))
            {
                ready.push(id.clone());
            }
        }
        while let Some(id) = ready.pop() {
            let Some(node) = pending.remove(&id) else {
                continue;
            };
            nodes.insert(id.clone(), node);
            if let Some(children) = waiting_by_parent.get(&Some(id)) {
                ready.extend(children.iter().cloned());
            }
        }

        let pending_bytes: usize = pending.iter().map(|(id, node)| node_charge(id, node)).sum();
        if nodes.len().saturating_add(pending.len()) > self.limits.max_nodes
            || pending.len() > self.limits.max_pending_nodes
            || pending_bytes > self.limits.max_pending_bytes
        {
            return Err(Error::ResourceLimit);
        }
        tombstones.extend(delta.tombstones.iter().cloned());
        if tombstones.len() > self.limits.max_tombstones {
            return Err(Error::ResourceLimit);
        }

        let mut clock = self.clock.clone();
        if let Some(greatest) = greatest_tag(delta) {
            clock.witness(greatest, physical_millis)?;
        }
        self.nodes = nodes;
        self.pending = pending;
        self.tombstones = tombstones;
        self.clock = clock;
        Ok(())
    }

    fn visible_positions(&self) -> Vec<Position> {
        let mut children: BTreeMap<Option<Position>, Vec<Position>> = BTreeMap::new();
        for (id, node) in &self.nodes {
            children
                .entry(node.parent.clone())
                .or_default()
                .push(id.clone());
        }
        let mut result = Vec::new();
        let mut stack = children.remove(&None).unwrap_or_default();
        while let Some(id) = stack.pop() {
            if !self.tombstones.contains(&id) {
                result.push(id.clone());
            }
            if let Some(descendants) = children.remove(&Some(id)) {
                stack.extend(descendants);
            }
        }
        result
    }
}

#[derive(Debug)]
struct Frame {
    type_id: u64,
    codec: Vec<u8>,
    payload: Vec<u8>,
}

fn decode_frame(data: &[u8], limits: Limits) -> Result<Frame, Error> {
    if !limits.valid() || data.len() > limits.max_frame_bytes || data.len() < 9 {
        return Err(Error::ResourceLimit);
    }
    if data.get(..4) != Some(b"CRDT") {
        return Err(Error::InvalidFrame);
    }
    let body_end = data.len() - 4;
    let expected = u32::from_be_bytes(
        data[body_end..]
            .try_into()
            .map_err(|_| Error::InvalidFrame)?,
    );
    if crc32c(&data[4..body_end]) != expected {
        return Err(Error::InvalidFrame);
    }
    let mut cursor = 4;
    let version = read_uvarint(data, &mut cursor, body_end)?;
    if version != FORMAT_VERSION {
        return Err(Error::ProtocolMismatch);
    }
    let type_id = read_uvarint(data, &mut cursor, body_end)?;
    if type_id == 0 {
        return Err(Error::InvalidFrame);
    }
    let codec = read_bytes(data, &mut cursor, body_end, limits.max_string_bytes)?;
    let payload = read_bytes(data, &mut cursor, body_end, limits.max_payload_bytes)?;
    if cursor != body_end {
        return Err(Error::InvalidFrame);
    }
    Ok(Frame {
        type_id,
        codec,
        payload,
    })
}

fn decode_payload(
    payload: &[u8],
    type_id: u64,
    complete: bool,
    limits: Limits,
) -> Result<Delta, Error> {
    let mut cursor = 0;
    let count = usize::try_from(read_uvarint(payload, &mut cursor, payload.len())?)
        .map_err(|_| Error::ResourceLimit)?;
    if count > limits.max_nodes {
        return Err(Error::ResourceLimit);
    }
    let mut nodes = BTreeMap::new();
    for _ in 0..count {
        match read_uvarint(payload, &mut cursor, payload.len())? {
            BLOCK_NODE => {
                let (id, node) = read_node(payload, &mut cursor, limits)?;
                if nodes.insert(id, node).is_some() {
                    return Err(Error::InvalidFrame);
                }
            }
            BLOCK_CHAIN => {
                let chain_count =
                    usize::try_from(read_uvarint(payload, &mut cursor, payload.len())?)
                        .map_err(|_| Error::ResourceLimit)?;
                if chain_count < 2 || chain_count > limits.max_nodes.saturating_sub(nodes.len()) {
                    return Err(Error::ResourceLimit);
                }
                let replica_id =
                    read_bytes(payload, &mut cursor, payload.len(), limits.max_string_bytes)?;
                let prototype = Position {
                    replica_id: replica_id.clone(),
                    wall_time: 0,
                    logical: 0,
                };
                if !prototype.valid() {
                    return Err(Error::InvalidFrame);
                }
                let mut parent = read_optional_position(payload, &mut cursor, limits)?;
                for _ in 0..chain_count {
                    let wall_time = read_uvarint(payload, &mut cursor, payload.len())?;
                    let logical = read_uvarint(payload, &mut cursor, payload.len())?;
                    let scalar = read_uvarint(payload, &mut cursor, payload.len())?;
                    let value =
                        char::from_u32(u32::try_from(scalar).map_err(|_| Error::InvalidFrame)?)
                            .ok_or(Error::InvalidFrame)?;
                    let id = Position {
                        replica_id: replica_id.clone(),
                        wall_time,
                        logical,
                    };
                    if nodes
                        .insert(
                            id.clone(),
                            Node {
                                parent: parent.clone(),
                                value,
                            },
                        )
                        .is_some()
                    {
                        return Err(Error::InvalidFrame);
                    }
                    parent = Some(id);
                }
            }
            _ => return Err(Error::InvalidFrame),
        }
    }
    let tomb_count = usize::try_from(read_uvarint(payload, &mut cursor, payload.len())?)
        .map_err(|_| Error::ResourceLimit)?;
    if tomb_count > limits.max_tags.saturating_sub(nodes.len()) {
        return Err(Error::ResourceLimit);
    }
    let mut tombstones = BTreeSet::new();
    for _ in 0..tomb_count {
        if !tombstones.insert(read_position(payload, &mut cursor, limits)?) {
            return Err(Error::InvalidFrame);
        }
    }
    if cursor != payload.len() {
        return Err(Error::InvalidFrame);
    }
    let delta = Delta { nodes, tombstones };
    validate_delta(&delta)?;
    validate_graph(&delta.nodes, complete)?;
    let canonical =
        encode_payload_frame(type_id, &delta.nodes, &delta.tombstones, complete, limits)?;
    let canonical_frame = decode_frame(&canonical, limits)?;
    if canonical_frame.payload != payload {
        return Err(Error::InvalidFrame);
    }
    Ok(delta)
}

fn read_node(data: &[u8], cursor: &mut usize, limits: Limits) -> Result<(Position, Node), Error> {
    let id = read_position(data, cursor, limits)?;
    let parent = read_optional_position(data, cursor, limits)?;
    let value = char::from_u32(
        u32::try_from(read_uvarint(data, cursor, data.len())?).map_err(|_| Error::InvalidFrame)?,
    )
    .ok_or(Error::InvalidFrame)?;
    Ok((id, Node { parent, value }))
}

fn read_optional_position(
    data: &[u8],
    cursor: &mut usize,
    limits: Limits,
) -> Result<Option<Position>, Error> {
    match read_uvarint(data, cursor, data.len())? {
        0 => Ok(None),
        1 => Ok(Some(read_position(data, cursor, limits)?)),
        _ => Err(Error::InvalidFrame),
    }
}

fn read_position(data: &[u8], cursor: &mut usize, limits: Limits) -> Result<Position, Error> {
    let replica_id = read_bytes(data, cursor, data.len(), limits.max_string_bytes)?;
    let wall_time = read_uvarint(data, cursor, data.len())?;
    let logical = read_uvarint(data, cursor, data.len())?;
    Position::new(replica_id, wall_time, logical).map_err(|_| Error::InvalidFrame)
}

fn read_bytes(
    data: &[u8],
    cursor: &mut usize,
    end: usize,
    maximum: usize,
) -> Result<Vec<u8>, Error> {
    let length =
        usize::try_from(read_uvarint(data, cursor, end)?).map_err(|_| Error::ResourceLimit)?;
    if length > maximum || length > end.saturating_sub(*cursor) {
        return Err(Error::ResourceLimit);
    }
    let next = cursor.checked_add(length).ok_or(Error::ResourceLimit)?;
    let value = data.get(*cursor..next).ok_or(Error::InvalidFrame)?.to_vec();
    *cursor = next;
    Ok(value)
}

fn read_uvarint(data: &[u8], cursor: &mut usize, end: usize) -> Result<u64, Error> {
    if *cursor >= end {
        return Err(Error::InvalidFrame);
    }
    let start = *cursor;
    let mut value = 0_u64;
    for shift in 0..10 {
        let byte = *data.get(*cursor).ok_or(Error::InvalidFrame)?;
        *cursor += 1;
        if shift == 9 && byte > 1 {
            return Err(Error::InvalidFrame);
        }
        value |= u64::from(byte & 0x7f) << (shift * 7);
        if byte & 0x80 == 0 {
            if *cursor - start != uvarint_size(value) {
                return Err(Error::InvalidFrame);
            }
            return Ok(value);
        }
        if *cursor >= end {
            return Err(Error::InvalidFrame);
        }
    }
    Err(Error::InvalidFrame)
}

fn encode_payload_frame(
    type_id: u64,
    nodes: &BTreeMap<Position, Node>,
    tombstones: &BTreeSet<Position>,
    complete: bool,
    limits: Limits,
) -> Result<Vec<u8>, Error> {
    if !limits.valid() || (type_id != RGA_RUN_STATE_TYPE_ID && type_id != RGA_RUN_DELTA_TYPE_ID) {
        return Err(Error::ProtocolMismatch);
    }
    validate_parts(nodes, tombstones)?;
    validate_graph(nodes, complete)?;
    if nodes.len() > limits.max_nodes
        || nodes.len() > limits.max_tags
        || tombstones.len() > limits.max_tags.saturating_sub(nodes.len())
    {
        return Err(Error::ResourceLimit);
    }
    let blocks = canonical_blocks(nodes);
    let mut payload = Vec::new();
    append_uvarint(
        &mut payload,
        u64::try_from(blocks.len()).map_err(|_| Error::ResourceLimit)?,
    );
    for block in blocks {
        if block.len() == 1 {
            append_uvarint(&mut payload, BLOCK_NODE);
            append_node(&mut payload, block[0].0, block[0].1);
        } else {
            append_uvarint(&mut payload, BLOCK_CHAIN);
            append_uvarint(
                &mut payload,
                u64::try_from(block.len()).map_err(|_| Error::ResourceLimit)?,
            );
            append_bytes(&mut payload, &block[0].0.replica_id);
            append_optional_position(&mut payload, block[0].1.parent.as_ref());
            for (id, node) in block {
                append_uvarint(&mut payload, id.wall_time);
                append_uvarint(&mut payload, id.logical);
                append_uvarint(&mut payload, u64::from(u32::from(node.value)));
            }
        }
        if payload.len() > limits.max_payload_bytes {
            return Err(Error::ResourceLimit);
        }
    }
    append_uvarint(
        &mut payload,
        u64::try_from(tombstones.len()).map_err(|_| Error::ResourceLimit)?,
    );
    for id in tombstones {
        append_position(&mut payload, id);
        if payload.len() > limits.max_payload_bytes {
            return Err(Error::ResourceLimit);
        }
    }
    encode_frame(type_id, &payload, limits)
}

fn canonical_blocks(nodes: &BTreeMap<Position, Node>) -> Vec<Vec<(&Position, &Node)>> {
    let mut successors: HashMap<(&Position, &[u8]), Vec<&Position>> = HashMap::new();
    for (id, node) in nodes {
        if let Some(parent) = &node.parent {
            successors
                .entry((parent, &id.replica_id))
                .or_default()
                .push(id);
        }
    }
    let mut used: HashSet<&Position> = HashSet::new();
    let mut blocks = Vec::new();
    for start in nodes.keys() {
        if used.contains(start) {
            continue;
        }
        let replica_id = start.replica_id.as_slice();
        let mut block = Vec::new();
        let mut current = start;
        loop {
            let node = nodes.get(current).expect("canonical block id must exist");
            used.insert(current);
            block.push((current, node));
            let next = successors.get(&(current, replica_id));
            let Some(next) = next else { break };
            if next.len() != 1 || used.contains(&next[0]) {
                break;
            }
            current = next[0];
        }
        blocks.push(block);
    }
    blocks
}

fn append_node(output: &mut Vec<u8>, id: &Position, node: &Node) {
    append_position(output, id);
    append_optional_position(output, node.parent.as_ref());
    append_uvarint(output, u64::from(u32::from(node.value)));
}

fn append_optional_position(output: &mut Vec<u8>, position: Option<&Position>) {
    if let Some(position) = position {
        append_uvarint(output, 1);
        append_position(output, position);
    } else {
        append_uvarint(output, 0);
    }
}

fn append_position(output: &mut Vec<u8>, position: &Position) {
    append_bytes(output, &position.replica_id);
    append_uvarint(output, position.wall_time);
    append_uvarint(output, position.logical);
}

fn append_bytes(output: &mut Vec<u8>, value: &[u8]) {
    append_uvarint(output, u64::try_from(value.len()).expect("usize fits u64"));
    output.extend_from_slice(value);
}

fn encode_frame(type_id: u64, payload: &[u8], limits: Limits) -> Result<Vec<u8>, Error> {
    if type_id == 0 || payload.len() > limits.max_payload_bytes {
        return Err(Error::ResourceLimit);
    }
    let full_length = 4
        + uvarint_size(FORMAT_VERSION)
        + uvarint_size(type_id)
        + 1
        + uvarint_size(u64::try_from(payload.len()).map_err(|_| Error::ResourceLimit)?)
        + payload.len()
        + 4;
    if full_length > limits.max_frame_bytes {
        return Err(Error::ResourceLimit);
    }
    let mut output = Vec::with_capacity(full_length);
    output.extend_from_slice(b"CRDT");
    append_uvarint(&mut output, FORMAT_VERSION);
    append_uvarint(&mut output, type_id);
    append_uvarint(&mut output, 0);
    append_bytes(&mut output, payload);
    output.extend_from_slice(&crc32c(&output[4..]).to_be_bytes());
    Ok(output)
}

fn append_uvarint(output: &mut Vec<u8>, mut value: u64) {
    while value >= 0x80 {
        output.push((value as u8) | 0x80);
        value >>= 7;
    }
    output.push(value as u8);
}

fn uvarint_size(mut value: u64) -> usize {
    let mut size = 1;
    while value >= 0x80 {
        value >>= 7;
        size += 1;
    }
    size
}

fn validate_delta(delta: &Delta) -> Result<(), Error> {
    validate_parts(&delta.nodes, &delta.tombstones)
}

fn validate_parts(
    nodes: &BTreeMap<Position, Node>,
    tombstones: &BTreeSet<Position>,
) -> Result<(), Error> {
    for (id, node) in nodes {
        if !id.valid()
            || node
                .parent
                .as_ref()
                .is_some_and(|parent| !parent.valid() || parent == id)
        {
            return Err(Error::InvalidDelta);
        }
    }
    if tombstones.iter().any(|id| !id.valid()) {
        return Err(Error::InvalidDelta);
    }
    Ok(())
}

fn validate_graph(nodes: &BTreeMap<Position, Node>, complete: bool) -> Result<(), Error> {
    validate_graph_inner(nodes, &BTreeMap::new(), complete)
}

fn validate_graph_with_integrated(
    pending: &BTreeMap<Position, Node>,
    integrated: &BTreeMap<Position, Node>,
) -> Result<(), Error> {
    validate_graph_inner(pending, integrated, false)
}

fn validate_graph_inner(
    nodes: &BTreeMap<Position, Node>,
    integrated: &BTreeMap<Position, Node>,
    complete: bool,
) -> Result<(), Error> {
    let mut marks: HashMap<Position, u8> = HashMap::new();
    for start in nodes.keys() {
        if matches!(marks.get(start), Some(2)) {
            continue;
        }
        let mut path = Vec::new();
        let mut current = start.clone();
        loop {
            match marks.get(&current) {
                Some(1) => return Err(Error::InvalidDelta),
                Some(2) => break,
                _ => {}
            }
            let Some(node) = nodes.get(&current) else {
                if complete && !integrated.contains_key(&current) {
                    return Err(Error::IncompleteState);
                }
                break;
            };
            marks.insert(current.clone(), 1);
            path.push(current.clone());
            let Some(parent) = &node.parent else { break };
            if complete && !nodes.contains_key(parent) && !integrated.contains_key(parent) {
                return Err(Error::IncompleteState);
            }
            if integrated.contains_key(parent) {
                break;
            }
            current = parent.clone();
        }
        for position in path {
            marks.insert(position, 2);
        }
    }
    Ok(())
}

fn greatest_tag(delta: &Delta) -> Option<&Position> {
    delta
        .nodes
        .iter()
        .flat_map(|(id, node)| std::iter::once(id).chain(node.parent.as_ref()))
        .chain(delta.tombstones.iter())
        .max()
}

fn node_charge(id: &Position, node: &Node) -> usize {
    64 + id.replica_id.len()
        + node
            .parent
            .as_ref()
            .map_or(0, |parent| parent.replica_id.len())
}

fn current_millis() -> Result<u64, Error> {
    let duration = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| Error::ClockExhausted)?;
    u64::try_from(duration.as_millis()).map_err(|_| Error::ClockExhausted)
}

fn crc32c(data: &[u8]) -> u32 {
    let table = CRC32C_TABLE.get_or_init(|| {
        let mut table = [0_u32; 256];
        for (index, entry) in table.iter_mut().enumerate() {
            let mut value = index as u32;
            for _ in 0..8 {
                value = if value & 1 != 0 {
                    0x82f6_3b78 ^ (value >> 1)
                } else {
                    value >> 1
                };
            }
            *entry = value;
        }
        table
    });
    let mut value = !0_u32;
    for byte in data {
        value = table[((value ^ u32::from(*byte)) & 0xff) as usize] ^ (value >> 8);
    }
    !value
}

static CRC32C_TABLE: OnceLock<[u32; 256]> = OnceLock::new();
