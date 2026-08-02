//! A bounded native implementation of the stable `lww-map-v1` protocol.
//!
//! Map values are opaque bytes. Applications must bind their own value schema
//! in the authenticated group manifest before accepting a frame. This module
//! deliberately implements only TypeIDs 9/10; it must not be used as a
//! fallback decoder for LWW-Set, attachments, or an application-defined map.

use std::collections::BTreeMap;
use std::str;

use crate::{
    ClockState, Error, Hlc, LWW_MAP_DELTA_TYPE_ID, LWW_MAP_STATE_TYPE_ID, Position, append_bytes,
    append_position, append_uvarint, current_millis, decode_frame, encode_frame, read_bytes,
    read_position, read_uvarint,
};

/// Receiver and retained-state bounds for [`LwwMap`].
///
/// The selected values are part of replication-group admission, not a local
/// hint. The host must authenticate the manifest that carries them before a
/// frame reaches this runtime.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct LwwMapLimits {
    /// Maximum complete frame bytes accepted or emitted.
    pub max_frame_bytes: usize,
    /// Maximum decoded map payload bytes accepted or emitted.
    pub max_payload_bytes: usize,
    /// Maximum UTF-8 key, opaque value, or replica-ID bytes.
    pub max_string_bytes: usize,
    /// Maximum retained entries, including delete tombstones.
    pub max_entries: usize,
    /// Maximum retained delete tombstones.
    pub max_tombstones: usize,
}

impl Default for LwwMapLimits {
    fn default() -> Self {
        Self {
            max_frame_bytes: 1 << 20,
            max_payload_bytes: 1 << 20,
            max_string_bytes: 64 << 10,
            max_entries: 100_000,
            max_tombstones: 100_000,
        }
    }
}

impl LwwMapLimits {
    pub(crate) fn valid(self) -> bool {
        self.max_frame_bytes > 0
            && self.max_payload_bytes > 0
            && self.max_string_bytes > 0
            && self.max_entries > 0
            && self.max_tombstones > 0
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct Entry {
    tag: Position,
    present: bool,
    value: Vec<u8>,
}

/// A joinable partial LWW-Map state. It is canonical only after [`LwwMapDelta::encode`].
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct LwwMapDelta {
    entries: BTreeMap<String, Entry>,
}

impl LwwMapDelta {
    /// Decodes one canonical, bounded TypeID 10 delta frame.
    pub fn decode(bytes: &[u8], limits: LwwMapLimits) -> Result<Self, Error> {
        let frame = decode_map_frame(bytes, limits)?;
        if frame.type_id != LWW_MAP_DELTA_TYPE_ID {
            return Err(Error::ProtocolMismatch);
        }
        decode_entries(&frame.payload, frame.type_id, limits).map(|entries| Self { entries })
    }

    /// Encodes the delta as its one canonical TypeID 10 frame.
    pub fn encode(&self, limits: LwwMapLimits) -> Result<Vec<u8>, Error> {
        encode_entries(LWW_MAP_DELTA_TYPE_ID, &self.entries, limits)
    }

    /// Returns true when the delta does not carry an entry or tombstone.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }
}

/// A bounded local LWW-Map v1 replica interoperable with Go TypeIDs 9/10.
///
/// State stores one immutable HLC tag per key. The larger tag wins; a reuse of
/// one tag for different key/value data is rejected. Persist [`LwwMap::state`]
/// together with [`LwwMap::clock_state`] and the host-owned frontier/outbox
/// before reusing a replica ID.
#[derive(Clone, Debug)]
pub struct LwwMap {
    limits: LwwMapLimits,
    clock: Hlc,
    entries: BTreeMap<String, Entry>,
    tags: BTreeMap<Position, String>,
}

impl LwwMap {
    /// Creates an empty map with a fresh local HLC state.
    pub fn new(replica_id: impl Into<Vec<u8>>, limits: LwwMapLimits) -> Result<Self, Error> {
        Self::from_clock_state(
            ClockState {
                replica_id: replica_id.into(),
                wall_time: 0,
                logical: 0,
            },
            limits,
        )
    }

    /// Creates an empty map after restoring a persisted HLC state.
    pub fn from_clock_state(clock: ClockState, limits: LwwMapLimits) -> Result<Self, Error> {
        if !limits.valid() {
            return Err(Error::ResourceLimit);
        }
        if !valid_replica_id(&clock.replica_id) {
            return Err(Error::InvalidDelta);
        }
        Ok(Self {
            limits,
            clock: Hlc::new(clock)?,
            entries: BTreeMap::new(),
            tags: BTreeMap::new(),
        })
    }

    /// Returns the state that must be persisted with [`Self::state`].
    #[must_use]
    pub fn clock_state(&self) -> ClockState {
        self.clock.state.clone()
    }

    /// Returns the visible value for `key`, if it has not been deleted.
    #[must_use]
    pub fn get(&self, key: &str) -> Option<Vec<u8>> {
        self.entries
            .get(key)
            .filter(|entry| entry.present)
            .map(|entry| entry.value.clone())
    }

    /// Returns all visible keys in canonical byte-lexical order.
    #[must_use]
    pub fn keys(&self) -> Vec<String> {
        self.entries
            .iter()
            .filter(|(_, entry)| entry.present)
            .map(|(key, _)| key.clone())
            .collect()
    }

    /// Returns retained entry count, including delete tombstones.
    #[must_use]
    pub fn entry_count(&self) -> usize {
        self.entries.len()
    }

    /// Sets an opaque value and returns its already-applied canonical delta.
    pub fn set(&mut self, key: &str, value: &[u8]) -> Result<Vec<u8>, Error> {
        self.set_at(key, value, current_millis()?)
    }

    /// Deterministic [`Self::set`] variant for tests and controlled hosts.
    pub fn set_at(
        &mut self,
        key: &str,
        value: &[u8],
        physical_millis: u64,
    ) -> Result<Vec<u8>, Error> {
        self.write_at(key, value, true, physical_millis)
    }

    /// Deletes `key` and returns its already-applied canonical delta.
    pub fn delete(&mut self, key: &str) -> Result<Vec<u8>, Error> {
        self.delete_at(key, current_millis()?)
    }

    /// Deterministic [`Self::delete`] variant for tests and controlled hosts.
    pub fn delete_at(&mut self, key: &str, physical_millis: u64) -> Result<Vec<u8>, Error> {
        self.write_at(key, &[], false, physical_millis)
    }

    /// Applies one authenticated, manifest-negotiated TypeID 9 or 10 frame.
    pub fn apply_frame(&mut self, bytes: &[u8]) -> Result<(), Error> {
        self.apply_frame_at(bytes, current_millis()?)
    }

    /// Deterministic [`Self::apply_frame`] variant for tests and controlled hosts.
    pub fn apply_frame_at(&mut self, bytes: &[u8], physical_millis: u64) -> Result<(), Error> {
        let frame = decode_map_frame(bytes, self.limits)?;
        match frame.type_id {
            LWW_MAP_DELTA_TYPE_ID => {
                let delta = LwwMapDelta {
                    entries: decode_entries(&frame.payload, frame.type_id, self.limits)?,
                };
                self.apply_delta_at(&delta, physical_millis)
            }
            LWW_MAP_STATE_TYPE_ID => {
                let entries = decode_entries(&frame.payload, frame.type_id, self.limits)?;
                self.install_state_at(entries, physical_millis)
            }
            _ => Err(Error::ProtocolMismatch),
        }
    }

    /// Returns the complete canonical TypeID 9 state frame.
    pub fn state(&self) -> Result<Vec<u8>, Error> {
        encode_entries(LWW_MAP_STATE_TYPE_ID, &self.entries, self.limits)
    }

    fn write_at(
        &mut self,
        key: &str,
        value: &[u8],
        present: bool,
        physical_millis: u64,
    ) -> Result<Vec<u8>, Error> {
        validate_write(key, value, present, self.limits)?;
        if !self.entries.contains_key(key) && self.entries.len() >= self.limits.max_entries {
            return Err(Error::ResourceLimit);
        }

        // Allocate a local tag and encode before mutation. A rejected local
        // write must leave both the visible map and persisted HLC unchanged.
        let mut clock = self.clock.clone();
        let tag = clock.next(physical_millis)?;
        if let Some(owner) = self.tags.get(&tag) {
            if owner != key {
                return Err(Error::TagConflict);
            }
        }
        let entry = Entry {
            tag: tag.clone(),
            present,
            value: if present { value.to_vec() } else { Vec::new() },
        };
        let mut delta_entries = BTreeMap::new();
        delta_entries.insert(key.to_owned(), entry.clone());
        let delta = LwwMapDelta {
            entries: delta_entries,
        };
        let encoded = delta.encode(self.limits)?;

        if let Some(previous) = self.entries.get(key) {
            if previous.tag >= tag {
                return Err(Error::TagConflict);
            }
        }
        if let Some(previous) = self.entries.insert(key.to_owned(), entry) {
            self.tags.remove(&previous.tag);
        }
        self.tags.insert(tag, key.to_owned());
        self.clock = clock;
        Ok(encoded)
    }

    fn install_state_at(
        &mut self,
        entries: BTreeMap<String, Entry>,
        physical_millis: u64,
    ) -> Result<(), Error> {
        enforce_entries(&entries, self.limits)?;
        let tags = tag_index(&entries)?;
        let mut clock = self.clock.clone();
        if let Some(tag) = greatest_tag(&entries) {
            clock.witness(tag, physical_millis)?;
        }
        self.entries = entries;
        self.tags = tags;
        self.clock = clock;
        Ok(())
    }

    fn apply_delta_at(&mut self, delta: &LwwMapDelta, physical_millis: u64) -> Result<(), Error> {
        enforce_entries(&delta.entries, self.limits)?;
        for (key, entry) in &delta.entries {
            if let Some(owner) = self.tags.get(&entry.tag) {
                if owner != key {
                    return Err(Error::TagConflict);
                }
            }
            if let Some(current) = self.entries.get(key) {
                if entries_conflict(current, entry) {
                    return Err(Error::TagConflict);
                }
            }
        }
        if entries_subsumed(&self.entries, &delta.entries) {
            return Ok(());
        }
        let added = delta
            .entries
            .keys()
            .filter(|key| !self.entries.contains_key(*key))
            .count();
        if self.entries.len().saturating_add(added) > self.limits.max_entries {
            return Err(Error::ResourceLimit);
        }

        let mut entries = self.entries.clone();
        let mut tags = self.tags.clone();
        for (key, incoming) in &delta.entries {
            let replaces = entries
                .get(key)
                .is_none_or(|current| current.tag < incoming.tag);
            if replaces {
                if let Some(previous) = entries.insert(key.clone(), incoming.clone()) {
                    tags.remove(&previous.tag);
                }
                tags.insert(incoming.tag.clone(), key.clone());
            }
        }
        enforce_entries(&entries, self.limits)?;
        let mut clock = self.clock.clone();
        if let Some(tag) = greatest_tag(&delta.entries) {
            clock.witness(tag, physical_millis)?;
        }
        self.entries = entries;
        self.tags = tags;
        self.clock = clock;
        Ok(())
    }
}

fn decode_map_frame(bytes: &[u8], limits: LwwMapLimits) -> Result<crate::Frame, Error> {
    if !limits.valid() || bytes.len() > limits.max_frame_bytes {
        return Err(Error::ResourceLimit);
    }
    // `decode_frame` is shared with run-v2. Map limits map onto its common
    // envelope/string fields; nodes/tags are checked by this module below.
    let frame_limits = crate::Limits {
        max_frame_bytes: limits.max_frame_bytes,
        max_payload_bytes: limits.max_payload_bytes,
        max_string_bytes: limits.max_string_bytes,
        max_nodes: limits.max_entries,
        max_tags: limits.max_entries,
        max_tombstones: limits.max_tombstones,
        max_pending_nodes: 1,
        max_pending_bytes: 1,
    };
    let frame = decode_frame(bytes, frame_limits)?;
    if !frame.codec.is_empty() {
        return Err(Error::ProtocolMismatch);
    }
    Ok(frame)
}

fn decode_entries(
    payload: &[u8],
    type_id: u64,
    limits: LwwMapLimits,
) -> Result<BTreeMap<String, Entry>, Error> {
    if type_id != LWW_MAP_STATE_TYPE_ID && type_id != LWW_MAP_DELTA_TYPE_ID {
        return Err(Error::ProtocolMismatch);
    }
    let mut cursor = 0;
    let count = usize::try_from(read_uvarint(payload, &mut cursor, payload.len())?)
        .map_err(|_| Error::ResourceLimit)?;
    if count > limits.max_entries {
        return Err(Error::ResourceLimit);
    }
    let mut entries = BTreeMap::new();
    let mut owners = BTreeMap::<Position, String>::new();
    let mut tombstones = 0usize;
    for _ in 0..count {
        let raw_key = read_bytes(payload, &mut cursor, payload.len(), limits.max_string_bytes)?;
        let key = str::from_utf8(&raw_key)
            .map_err(|_| Error::InvalidFrame)?
            .to_owned();
        if !valid_key(&key) {
            return Err(Error::InvalidFrame);
        }
        let tag = read_position(payload, &mut cursor, shared_limits(limits))?;
        if !valid_replica_id(&tag.replica_id) {
            return Err(Error::InvalidFrame);
        }
        let present = read_uvarint(payload, &mut cursor, payload.len())?;
        if present > 1 {
            return Err(Error::InvalidFrame);
        }
        let value = if present == 1 {
            read_bytes(payload, &mut cursor, payload.len(), limits.max_string_bytes)?
        } else {
            tombstones = tombstones.saturating_add(1);
            Vec::new()
        };
        if tombstones > limits.max_tombstones {
            return Err(Error::ResourceLimit);
        }
        if let Some(owner) = owners.insert(tag.clone(), key.clone()) {
            if owner != key {
                return Err(Error::InvalidFrame);
            }
        }
        if entries
            .insert(
                key,
                Entry {
                    tag,
                    present: present == 1,
                    value,
                },
            )
            .is_some()
        {
            return Err(Error::InvalidFrame);
        }
    }
    if cursor != payload.len() {
        return Err(Error::InvalidFrame);
    }
    enforce_entries(&entries, limits)?;
    // A correctly parsed but non-canonical order or encoding is not admitted.
    let canonical = encode_entries(type_id, &entries, limits)?;
    let canonical_frame = decode_map_frame(&canonical, limits)?;
    if canonical_frame.payload != payload {
        return Err(Error::InvalidFrame);
    }
    Ok(entries)
}

fn encode_entries(
    type_id: u64,
    entries: &BTreeMap<String, Entry>,
    limits: LwwMapLimits,
) -> Result<Vec<u8>, Error> {
    if type_id != LWW_MAP_STATE_TYPE_ID && type_id != LWW_MAP_DELTA_TYPE_ID {
        return Err(Error::ProtocolMismatch);
    }
    enforce_entries(entries, limits)?;
    let mut payload_len = uvarint_size(entries.len() as u64);
    for (key, entry) in entries {
        checked_add(
            &mut payload_len,
            uvarint_size(key.len() as u64).saturating_add(key.len()),
            limits.max_payload_bytes,
        )?;
        checked_add(
            &mut payload_len,
            tag_size(&entry.tag),
            limits.max_payload_bytes,
        )?;
        checked_add(&mut payload_len, 1, limits.max_payload_bytes)?;
        if entry.present {
            checked_add(
                &mut payload_len,
                uvarint_size(entry.value.len() as u64).saturating_add(entry.value.len()),
                limits.max_payload_bytes,
            )?;
        }
    }
    let mut payload = Vec::with_capacity(payload_len);
    append_uvarint(&mut payload, entries.len() as u64);
    for (key, entry) in entries {
        append_bytes(&mut payload, key.as_bytes());
        append_position(&mut payload, &entry.tag);
        append_uvarint(&mut payload, u64::from(entry.present));
        if entry.present {
            append_bytes(&mut payload, &entry.value);
        }
    }
    if payload.len() != payload_len {
        return Err(Error::InvalidFrame);
    }
    encode_map_frame(type_id, &payload, limits)
}

fn encode_map_frame(type_id: u64, payload: &[u8], limits: LwwMapLimits) -> Result<Vec<u8>, Error> {
    let frame_limits = shared_limits(limits);
    encode_frame(type_id, payload, frame_limits)
}

fn shared_limits(limits: LwwMapLimits) -> crate::Limits {
    crate::Limits {
        max_frame_bytes: limits.max_frame_bytes,
        max_payload_bytes: limits.max_payload_bytes,
        max_string_bytes: limits.max_string_bytes,
        max_nodes: limits.max_entries,
        max_tags: limits.max_entries,
        max_tombstones: limits.max_tombstones,
        max_pending_nodes: 1,
        max_pending_bytes: 1,
    }
}

fn enforce_entries(entries: &BTreeMap<String, Entry>, limits: LwwMapLimits) -> Result<(), Error> {
    if !limits.valid() || entries.len() > limits.max_entries {
        return Err(Error::ResourceLimit);
    }
    let mut owners = BTreeMap::<Position, String>::new();
    let mut tombstones = 0usize;
    for (key, entry) in entries {
        validate_write(key, &entry.value, entry.present, limits)?;
        if !valid_replica_id(&entry.tag.replica_id) {
            return Err(Error::InvalidDelta);
        }
        if !entry.present {
            tombstones = tombstones.saturating_add(1);
        }
        if tombstones > limits.max_tombstones {
            return Err(Error::ResourceLimit);
        }
        if let Some(owner) = owners.insert(entry.tag.clone(), key.clone()) {
            if owner != *key {
                return Err(Error::TagConflict);
            }
        }
    }
    Ok(())
}

fn validate_write(
    key: &str,
    value: &[u8],
    present: bool,
    limits: LwwMapLimits,
) -> Result<(), Error> {
    if !valid_key(key) || key.len() > limits.max_string_bytes {
        return Err(Error::InvalidDelta);
    }
    if (!present && !value.is_empty()) || (present && value.len() > limits.max_string_bytes) {
        return Err(Error::ResourceLimit);
    }
    Ok(())
}

fn valid_key(key: &str) -> bool {
    !key.trim().is_empty()
}

fn valid_replica_id(value: &[u8]) -> bool {
    str::from_utf8(value).is_ok_and(|id| !id.trim().is_empty())
}

fn entries_conflict(left: &Entry, right: &Entry) -> bool {
    left.tag == right.tag && (left.present != right.present || left.value != right.value)
}

fn entries_subsumed(
    existing: &BTreeMap<String, Entry>,
    incoming: &BTreeMap<String, Entry>,
) -> bool {
    incoming.iter().all(|(key, entry)| {
        existing
            .get(key)
            .is_some_and(|current| current.tag >= entry.tag && !entries_conflict(current, entry))
    })
}

fn tag_index(entries: &BTreeMap<String, Entry>) -> Result<BTreeMap<Position, String>, Error> {
    enforce_entries(
        entries,
        LwwMapLimits {
            max_frame_bytes: usize::MAX,
            max_payload_bytes: usize::MAX,
            max_string_bytes: usize::MAX,
            max_entries: usize::MAX,
            max_tombstones: usize::MAX,
        },
    )?;
    Ok(entries
        .iter()
        .map(|(key, entry)| (entry.tag.clone(), key.clone()))
        .collect())
}

fn greatest_tag(entries: &BTreeMap<String, Entry>) -> Option<&Position> {
    entries.values().map(|entry| &entry.tag).max()
}

fn tag_size(tag: &Position) -> usize {
    uvarint_size(tag.replica_id.len() as u64)
        .saturating_add(tag.replica_id.len())
        .saturating_add(uvarint_size(tag.wall_time))
        .saturating_add(uvarint_size(tag.logical))
}

fn checked_add(total: &mut usize, additional: usize, maximum: usize) -> Result<(), Error> {
    *total = total.checked_add(additional).ok_or(Error::ResourceLimit)?;
    if *total > maximum {
        return Err(Error::ResourceLimit);
    }
    Ok(())
}

fn uvarint_size(mut value: u64) -> usize {
    let mut size = 1;
    while value >= 0x80 {
        value >>= 7;
        size += 1;
    }
    size
}
