//! Minimal C ABI used by the checked-in Python and Swift wrappers.
//!
//! The ABI accepts and returns owned byte buffers only. It never lends a Rust
//! slice across the boundary, so callers cannot retain an alias to mutable RGA
//! state. Every exported operation maps malformed data and resource rejection
//! to an error code instead of unwinding through foreign code.

use std::ptr;
use std::slice;
use std::str;
use std::sync::Mutex;

use crate::{ClockState, Error, Limits, LwwMap, LwwMapLimits, Rga};

const OK: i32 = 0;
const INVALID_ARGUMENT: i32 = 1;
const INVALID_FRAME: i32 = 2;
const RESOURCE_LIMIT: i32 = 3;
const PROTOCOL_MISMATCH: i32 = 4;
const INVALID_DELTA: i32 = 5;
const RANGE: i32 = 6;
const INTERNAL: i32 = 7;

/// A byte buffer allocated by this library. Call `crdt_buffer_free` exactly
/// once after consuming any successful output.
#[repr(C)]
pub struct CrdtBuffer {
    pub data: *mut u8,
    pub len: usize,
}

/// A C-owned representation of persistable HLC state.
#[repr(C)]
pub struct CrdtClockState {
    pub replica_id: CrdtBuffer,
    pub wall_time: u64,
    pub logical: u64,
}

impl CrdtClockState {
    const fn empty() -> Self {
        Self {
            replica_id: CrdtBuffer::empty(),
            wall_time: 0,
            logical: 0,
        }
    }
}

impl CrdtBuffer {
    const fn empty() -> Self {
        Self {
            data: ptr::null_mut(),
            len: 0,
        }
    }

    fn from_vec(value: Vec<u8>) -> Self {
        let mut value = value.into_boxed_slice();
        let output = Self {
            data: value.as_mut_ptr(),
            len: value.len(),
        };
        std::mem::forget(value);
        output
    }
}

/// ABI-stable limits for `crdt_rga_new_with_limits`.
#[repr(C)]
#[derive(Clone, Copy)]
pub struct CrdtLimits {
    pub max_frame_bytes: usize,
    pub max_payload_bytes: usize,
    pub max_string_bytes: usize,
    pub max_nodes: usize,
    pub max_tags: usize,
    pub max_tombstones: usize,
    pub max_pending_nodes: usize,
    pub max_pending_bytes: usize,
}

impl From<CrdtLimits> for Limits {
    fn from(value: CrdtLimits) -> Self {
        Self {
            max_frame_bytes: value.max_frame_bytes,
            max_payload_bytes: value.max_payload_bytes,
            max_string_bytes: value.max_string_bytes,
            max_nodes: value.max_nodes,
            max_tags: value.max_tags,
            max_tombstones: value.max_tombstones,
            max_pending_nodes: value.max_pending_nodes,
            max_pending_bytes: value.max_pending_bytes,
        }
    }
}

/// ABI-stable limits for `crdt_lww_map_new_with_limits`.
#[repr(C)]
#[derive(Clone, Copy)]
pub struct CrdtLwwMapLimits {
    pub max_frame_bytes: usize,
    pub max_payload_bytes: usize,
    pub max_string_bytes: usize,
    pub max_entries: usize,
    pub max_tombstones: usize,
}

impl From<CrdtLwwMapLimits> for LwwMapLimits {
    fn from(value: CrdtLwwMapLimits) -> Self {
        Self {
            max_frame_bytes: value.max_frame_bytes,
            max_payload_bytes: value.max_payload_bytes,
            max_string_bytes: value.max_string_bytes,
            max_entries: value.max_entries,
            max_tombstones: value.max_tombstones,
        }
    }
}

/// Opaque, mutex-protected RGA handle. A handle is safe for one wrapper to use
/// from several threads, but callers must not use it after `crdt_rga_free`.
pub struct CrdtRga {
    inner: Mutex<Rga>,
}

/// Opaque, mutex-protected LWW-Map handle. A handle is safe for one wrapper
/// to use from several threads, but callers must not use it after free.
pub struct CrdtLwwMap {
    inner: Mutex<LwwMap>,
}

fn code(error: &Error) -> i32 {
    match error {
        Error::InvalidFrame => INVALID_FRAME,
        Error::ResourceLimit | Error::IncompleteState | Error::ClockExhausted => RESOURCE_LIMIT,
        Error::ProtocolMismatch => PROTOCOL_MISMATCH,
        Error::InvalidDelta | Error::TagConflict => INVALID_DELTA,
        Error::Range => RANGE,
    }
}

unsafe fn input<'a>(data: *const u8, len: usize) -> Result<&'a [u8], i32> {
    if data.is_null() && len != 0 {
        return Err(INVALID_ARGUMENT);
    }
    if len == 0 {
        return Ok(&[]);
    }
    // SAFETY: callers promise a non-null pointer to `len` initialized bytes;
    // the public ABI checks the one null-pointer case it can establish here.
    Ok(unsafe { slice::from_raw_parts(data, len) })
}

unsafe fn handle<'a>(value: *mut CrdtRga) -> Result<&'a CrdtRga, i32> {
    if value.is_null() {
        return Err(INVALID_ARGUMENT);
    }
    // SAFETY: handle ownership is created by `crdt_rga_new*` and remains valid
    // until its matching `crdt_rga_free` call.
    Ok(unsafe { &*value })
}

unsafe fn lww_map_handle<'a>(value: *mut CrdtLwwMap) -> Result<&'a CrdtLwwMap, i32> {
    if value.is_null() {
        return Err(INVALID_ARGUMENT);
    }
    // SAFETY: handle ownership is created by `crdt_lww_map_new*` and remains
    // valid until its matching `crdt_lww_map_free` call.
    Ok(unsafe { &*value })
}

unsafe fn write_output(output: *mut CrdtBuffer, value: Vec<u8>) -> Result<(), i32> {
    if output.is_null() {
        return Err(INVALID_ARGUMENT);
    }
    // SAFETY: the caller supplied writable storage for one CrdtBuffer.
    unsafe {
        *output = CrdtBuffer::from_vec(value);
    }
    Ok(())
}

/// Creates a default-bounded RGA from a UTF-8 replica ID.
///
/// # Safety
/// `replica` must be null only when `replica_len` is zero, and `out` must
/// point to writable storage for one handle pointer.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_rga_new(
    replica: *const u8,
    replica_len: usize,
    out: *mut *mut CrdtRga,
) -> i32 {
    // SAFETY: delegated to the checked input helper; output is validated below.
    unsafe { crdt_rga_new_inner(replica, replica_len, Limits::default(), out) }
}

/// Creates an RGA using application-negotiated bounds.
///
/// # Safety
/// The pointers follow the same requirements as [`crdt_rga_new`]; `limits`
/// must point to one initialized [`CrdtLimits`].
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_rga_new_with_limits(
    replica: *const u8,
    replica_len: usize,
    limits: *const CrdtLimits,
    out: *mut *mut CrdtRga,
) -> i32 {
    if limits.is_null() {
        return INVALID_ARGUMENT;
    }
    // SAFETY: `limits` was checked non-null and the ABI requires initialized storage.
    let selected = unsafe { Limits::from(*limits) };
    // SAFETY: delegated to the checked input helper; output is validated below.
    unsafe { crdt_rga_new_inner(replica, replica_len, selected, out) }
}

/// Restores an empty RGA with a persisted HLC state. Install the matching
/// complete state frame before producing a local mutation.
///
/// # Safety
/// The pointers follow the same requirements as [`crdt_rga_new`].
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_rga_new_from_clock(
    replica: *const u8,
    replica_len: usize,
    wall_time: u64,
    logical: u64,
    out: *mut *mut CrdtRga,
) -> i32 {
    // SAFETY: delegated to the checked constructor helper.
    unsafe {
        crdt_rga_new_from_clock_inner(
            replica,
            replica_len,
            wall_time,
            logical,
            Limits::default(),
            out,
        )
    }
}

/// Restores an empty RGA with a persisted HLC state and application-negotiated
/// limits. Use the same limits that admitted the matching complete state frame
/// so recovery cannot silently widen a replication group's resource policy.
///
/// # Safety
/// The pointers follow the same requirements as [`crdt_rga_new`]; `limits`
/// must point to one initialized [`CrdtLimits`].
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_rga_new_from_clock_with_limits(
    replica: *const u8,
    replica_len: usize,
    wall_time: u64,
    logical: u64,
    limits: *const CrdtLimits,
    out: *mut *mut CrdtRga,
) -> i32 {
    if limits.is_null() {
        return INVALID_ARGUMENT;
    }
    // SAFETY: `limits` was checked non-null and the ABI requires initialized storage.
    let selected = unsafe { Limits::from(*limits) };
    // SAFETY: delegated to the checked constructor helper.
    unsafe {
        crdt_rga_new_from_clock_inner(replica, replica_len, wall_time, logical, selected, out)
    }
}

unsafe fn crdt_rga_new_from_clock_inner(
    replica: *const u8,
    replica_len: usize,
    wall_time: u64,
    logical: u64,
    limits: Limits,
    out: *mut *mut CrdtRga,
) -> i32 {
    if out.is_null() {
        return INVALID_ARGUMENT;
    }
    // SAFETY: checked by `input` against the ABI pointer contract.
    let replica = match unsafe { input(replica, replica_len) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let state = ClockState {
        replica_id: replica.to_vec(),
        wall_time,
        logical,
    };
    let value = match Rga::from_clock_state(state, limits) {
        Ok(value) => value,
        Err(error) => return code(&error),
    };
    // SAFETY: output pointer was checked non-null and points to writable storage by ABI contract.
    unsafe {
        *out = Box::into_raw(Box::new(CrdtRga {
            inner: Mutex::new(value),
        }));
    }
    OK
}

unsafe fn crdt_rga_new_inner(
    replica: *const u8,
    replica_len: usize,
    limits: Limits,
    out: *mut *mut CrdtRga,
) -> i32 {
    if out.is_null() {
        return INVALID_ARGUMENT;
    }
    // SAFETY: checked by `input` against the ABI pointer contract.
    let replica = match unsafe { input(replica, replica_len) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let Ok(value) = Rga::new(replica.to_vec(), limits) else {
        return INVALID_ARGUMENT;
    };
    // SAFETY: output pointer was checked non-null and points to writable storage by ABI contract.
    unsafe {
        *out = Box::into_raw(Box::new(CrdtRga {
            inner: Mutex::new(value),
        }));
    }
    OK
}

/// Releases a handle created by an RGA constructor.
/// Passing null is allowed; every non-null handle must be freed exactly once.
///
/// # Safety
/// A non-null handle must have been returned by this library and not freed.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_rga_free(value: *mut CrdtRga) {
    if !value.is_null() {
        // SAFETY: ownership is transferred back exactly once by the ABI contract.
        unsafe {
            drop(Box::from_raw(value));
        }
    }
}

/// Applies one bounded run-v2 state or delta frame.
///
/// # Safety
/// `value` is a live handle; `frame` follows the input pointer contract.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_rga_apply(
    value: *mut CrdtRga,
    frame: *const u8,
    frame_len: usize,
) -> i32 {
    // SAFETY: helpers validate public pointer invariants.
    let value = match unsafe { handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: helpers validate public pointer invariants.
    let frame = match unsafe { input(frame, frame_len) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    match value
        .inner
        .lock()
        .map_err(|_| INTERNAL)
        .and_then(|mut rga| rga.apply_frame(frame).map_err(|error| code(&error)))
    {
        Ok(()) => OK,
        Err(error) => error,
    }
}

/// Inserts UTF-8 text and returns its already-applied canonical delta frame.
///
/// # Safety
/// `value` is a live handle; `text` and `out` follow their respective ABI
/// pointer contracts.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_rga_insert(
    value: *mut CrdtRga,
    offset: usize,
    text: *const u8,
    text_len: usize,
    out: *mut CrdtBuffer,
) -> i32 {
    // SAFETY: initialized to an empty value before a later failure is reported.
    if !out.is_null() {
        unsafe {
            *out = CrdtBuffer::empty();
        }
    }
    // SAFETY: helpers validate public pointer invariants.
    let value = match unsafe { handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: helpers validate public pointer invariants.
    let bytes = match unsafe { input(text, text_len) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let text = match str::from_utf8(bytes) {
        Ok(value) => value,
        Err(_) => return INVALID_ARGUMENT,
    };
    let encoded = match value
        .inner
        .lock()
        .map_err(|_| INTERNAL)
        .and_then(|mut rga| rga.insert(offset, text).map_err(|error| code(&error)))
    {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: checked by write_output.
    match unsafe { write_output(out, encoded) } {
        Ok(()) => OK,
        Err(error) => error,
    }
}

/// Deletes a visible scalar range and returns its already-applied delta frame.
///
/// # Safety
/// `value` is a live handle and `out` points to writable buffer storage.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_rga_delete(
    value: *mut CrdtRga,
    offset: usize,
    count: usize,
    out: *mut CrdtBuffer,
) -> i32 {
    // SAFETY: initialized to an empty value before a later failure is reported.
    if !out.is_null() {
        unsafe {
            *out = CrdtBuffer::empty();
        }
    }
    // SAFETY: helpers validate public pointer invariants.
    let value = match unsafe { handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let encoded = match value
        .inner
        .lock()
        .map_err(|_| INTERNAL)
        .and_then(|mut rga| rga.delete(offset, count).map_err(|error| code(&error)))
    {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: checked by write_output.
    match unsafe { write_output(out, encoded) } {
        Ok(()) => OK,
        Err(error) => error,
    }
}

/// Writes a canonical complete state frame to `out`.
///
/// # Safety
/// `value` is a live handle and `out` points to writable buffer storage.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_rga_state(value: *mut CrdtRga, out: *mut CrdtBuffer) -> i32 {
    // SAFETY: initialized to an empty value before a later failure is reported.
    if !out.is_null() {
        unsafe {
            *out = CrdtBuffer::empty();
        }
    }
    // SAFETY: helpers validate public pointer invariants.
    let value = match unsafe { handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let encoded = match value
        .inner
        .lock()
        .map_err(|_| INTERNAL)
        .and_then(|rga| rga.encode_state().map_err(|error| code(&error)))
    {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: checked by write_output.
    match unsafe { write_output(out, encoded) } {
        Ok(()) => OK,
        Err(error) => error,
    }
}

/// Writes the HLC state that must be persisted with `crdt_rga_state`.
///
/// # Safety
/// `value` is a live handle and `out` points to writable clock-state storage.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_rga_clock_state(
    value: *mut CrdtRga,
    out: *mut CrdtClockState,
) -> i32 {
    if out.is_null() {
        return INVALID_ARGUMENT;
    }
    // SAFETY: output pointer was checked non-null and points to writable storage by ABI contract.
    unsafe {
        *out = CrdtClockState::empty();
    }
    // SAFETY: helper validates the public pointer invariant.
    let value = match unsafe { handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let state = match value.inner.lock().map_err(|_| INTERNAL) {
        Ok(rga) => rga.clock_state(),
        Err(error) => return error,
    };
    // SAFETY: output pointer remains valid for the call by ABI contract.
    unsafe {
        *out = CrdtClockState {
            replica_id: CrdtBuffer::from_vec(state.replica_id),
            wall_time: state.wall_time,
            logical: state.logical,
        };
    }
    OK
}

/// Writes the current visible UTF-8 text to `out`.
///
/// # Safety
/// `value` is a live handle and `out` points to writable buffer storage.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_rga_text(value: *mut CrdtRga, out: *mut CrdtBuffer) -> i32 {
    // SAFETY: initialized to an empty value before a later failure is reported.
    if !out.is_null() {
        unsafe {
            *out = CrdtBuffer::empty();
        }
    }
    // SAFETY: helpers validate public pointer invariants.
    let value = match unsafe { handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let output = match value.inner.lock().map_err(|_| INTERNAL) {
        Ok(rga) => rga.text().into_bytes(),
        Err(error) => return error,
    };
    // SAFETY: checked by write_output.
    match unsafe { write_output(out, output) } {
        Ok(()) => OK,
        Err(error) => error,
    }
}

/// Releases a buffer returned by insert, delete, state, or text. Passing an
/// empty/null buffer is allowed.
///
/// # Safety
/// A non-empty buffer must be returned by this library and freed once.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_buffer_free(value: CrdtBuffer) {
    if !value.data.is_null() {
        // SAFETY: `from_vec` allocates an owned boxed slice with this exact length.
        unsafe {
            drop(Box::from_raw(ptr::slice_from_raw_parts_mut(
                value.data, value.len,
            )));
        }
    }
}

/// Releases the replica-ID buffer inside a returned clock state.
///
/// # Safety
/// `value` must have been returned by `crdt_rga_clock_state` and freed once.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_clock_state_free(value: CrdtClockState) {
    // SAFETY: the nested buffer follows `crdt_buffer_free`'s ownership contract.
    unsafe {
        crdt_buffer_free(value.replica_id);
    }
}

/// Creates a default-bounded LWW-Map from a UTF-8 replica ID.
///
/// # Safety
/// `replica` must be null only when `replica_len` is zero, and `out` must
/// point to writable storage for one handle pointer.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_new(
    replica: *const u8,
    replica_len: usize,
    out: *mut *mut CrdtLwwMap,
) -> i32 {
    // SAFETY: delegated to the checked input helper; output is validated below.
    unsafe { crdt_lww_map_new_inner(replica, replica_len, LwwMapLimits::default(), out) }
}

/// Creates an LWW-Map using application-negotiated bounds.
///
/// # Safety
/// The pointers follow the same requirements as [`crdt_lww_map_new`]; `limits`
/// must point to one initialized [`CrdtLwwMapLimits`].
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_new_with_limits(
    replica: *const u8,
    replica_len: usize,
    limits: *const CrdtLwwMapLimits,
    out: *mut *mut CrdtLwwMap,
) -> i32 {
    if limits.is_null() {
        return INVALID_ARGUMENT;
    }
    // SAFETY: `limits` was checked non-null and the ABI requires initialized storage.
    let selected = LwwMapLimits::from(unsafe { *limits });
    // SAFETY: delegated to the checked input helper; output is validated below.
    unsafe { crdt_lww_map_new_inner(replica, replica_len, selected, out) }
}

/// Restores an empty LWW-Map with a persisted HLC state. Install the matching
/// complete state frame before producing a local mutation.
///
/// # Safety
/// The pointers follow the same requirements as [`crdt_lww_map_new`].
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_new_from_clock(
    replica: *const u8,
    replica_len: usize,
    wall_time: u64,
    logical: u64,
    out: *mut *mut CrdtLwwMap,
) -> i32 {
    // SAFETY: delegated to the checked input helper; output is validated below.
    unsafe {
        crdt_lww_map_new_from_clock_inner(
            replica,
            replica_len,
            wall_time,
            logical,
            LwwMapLimits::default(),
            out,
        )
    }
}

/// Restores an LWW-Map clock using application-negotiated bounds.
///
/// # Safety
/// The pointers follow the same requirements as [`crdt_lww_map_new`]; `limits`
/// must point to one initialized [`CrdtLwwMapLimits`].
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_new_from_clock_with_limits(
    replica: *const u8,
    replica_len: usize,
    wall_time: u64,
    logical: u64,
    limits: *const CrdtLwwMapLimits,
    out: *mut *mut CrdtLwwMap,
) -> i32 {
    if limits.is_null() {
        return INVALID_ARGUMENT;
    }
    // SAFETY: `limits` was checked non-null and the ABI requires initialized storage.
    let selected = LwwMapLimits::from(unsafe { *limits });
    // SAFETY: delegated to the checked input helper; output is validated below.
    unsafe {
        crdt_lww_map_new_from_clock_inner(replica, replica_len, wall_time, logical, selected, out)
    }
}

unsafe fn crdt_lww_map_new_from_clock_inner(
    replica: *const u8,
    replica_len: usize,
    wall_time: u64,
    logical: u64,
    limits: LwwMapLimits,
    out: *mut *mut CrdtLwwMap,
) -> i32 {
    if out.is_null() {
        return INVALID_ARGUMENT;
    }
    // SAFETY: checked by `input` against the ABI pointer contract.
    let replica = match unsafe { input(replica, replica_len) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    if str::from_utf8(replica).is_err() {
        return INVALID_ARGUMENT;
    }
    let state = ClockState {
        replica_id: replica.to_vec(),
        wall_time,
        logical,
    };
    let value = match LwwMap::from_clock_state(state, limits) {
        Ok(value) => value,
        Err(error) => return code(&error),
    };
    // SAFETY: output pointer was checked non-null and points to writable storage by ABI contract.
    unsafe {
        *out = Box::into_raw(Box::new(CrdtLwwMap {
            inner: Mutex::new(value),
        }));
    }
    OK
}

unsafe fn crdt_lww_map_new_inner(
    replica: *const u8,
    replica_len: usize,
    limits: LwwMapLimits,
    out: *mut *mut CrdtLwwMap,
) -> i32 {
    if out.is_null() {
        return INVALID_ARGUMENT;
    }
    // SAFETY: checked by `input` against the ABI pointer contract.
    let replica = match unsafe { input(replica, replica_len) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    if str::from_utf8(replica).is_err() {
        return INVALID_ARGUMENT;
    }
    let value = match LwwMap::new(replica.to_vec(), limits) {
        Ok(value) => value,
        Err(error) => return code(&error),
    };
    // SAFETY: output pointer was checked non-null and points to writable storage by ABI contract.
    unsafe {
        *out = Box::into_raw(Box::new(CrdtLwwMap {
            inner: Mutex::new(value),
        }));
    }
    OK
}

/// Releases a handle created by `crdt_lww_map_new*`.
/// Passing null is allowed; every non-null handle must be freed exactly once.
///
/// # Safety
/// A non-null handle must have been returned by this library and not freed.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_free(value: *mut CrdtLwwMap) {
    if !value.is_null() {
        // SAFETY: ownership is transferred back exactly once by the ABI contract.
        unsafe {
            drop(Box::from_raw(value));
        }
    }
}

/// Applies one bounded LWW-Map v1 state or delta frame.
///
/// # Safety
/// `value` is a live handle; `frame` follows the input pointer contract.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_apply(
    value: *mut CrdtLwwMap,
    frame: *const u8,
    frame_len: usize,
) -> i32 {
    // SAFETY: helpers validate public pointer invariants.
    let value = match unsafe { lww_map_handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: helpers validate public pointer invariants.
    let frame = match unsafe { input(frame, frame_len) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    match value
        .inner
        .lock()
        .map_err(|_| INTERNAL)
        .and_then(|mut map| map.apply_frame(frame).map_err(|error| code(&error)))
    {
        Ok(()) => OK,
        Err(error) => error,
    }
}

/// Sets one UTF-8 key to an opaque value and returns its already-applied delta.
///
/// # Safety
/// `value` is a live handle; `key`, `bytes`, and `out` follow their pointer contracts.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_set(
    value: *mut CrdtLwwMap,
    key: *const u8,
    key_len: usize,
    bytes: *const u8,
    bytes_len: usize,
    out: *mut CrdtBuffer,
) -> i32 {
    initialize_output(out);
    // SAFETY: helpers validate public pointer invariants.
    let value = match unsafe { lww_map_handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: helpers validate public pointer invariants.
    let key = match unsafe { input(key, key_len) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let key = match str::from_utf8(key) {
        Ok(value) => value,
        Err(_) => return INVALID_ARGUMENT,
    };
    // SAFETY: helpers validate public pointer invariants.
    let bytes = match unsafe { input(bytes, bytes_len) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let encoded = match value
        .inner
        .lock()
        .map_err(|_| INTERNAL)
        .and_then(|mut map| map.set(key, bytes).map_err(|error| code(&error)))
    {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: checked by write_output.
    match unsafe { write_output(out, encoded) } {
        Ok(()) => OK,
        Err(error) => error,
    }
}

/// Deletes one UTF-8 key and returns its already-applied delta.
///
/// # Safety
/// `value` is a live handle; `key` and `out` follow their pointer contracts.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_delete(
    value: *mut CrdtLwwMap,
    key: *const u8,
    key_len: usize,
    out: *mut CrdtBuffer,
) -> i32 {
    initialize_output(out);
    // SAFETY: helpers validate public pointer invariants.
    let value = match unsafe { lww_map_handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: helpers validate public pointer invariants.
    let key = match unsafe { input(key, key_len) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let key = match str::from_utf8(key) {
        Ok(value) => value,
        Err(_) => return INVALID_ARGUMENT,
    };
    let encoded = match value
        .inner
        .lock()
        .map_err(|_| INTERNAL)
        .and_then(|mut map| map.delete(key).map_err(|error| code(&error)))
    {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: checked by write_output.
    match unsafe { write_output(out, encoded) } {
        Ok(()) => OK,
        Err(error) => error,
    }
}

/// Writes a canonical complete TypeID 9 state frame to `out`.
///
/// # Safety
/// `value` is a live handle and `out` points to writable buffer storage.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_state(value: *mut CrdtLwwMap, out: *mut CrdtBuffer) -> i32 {
    initialize_output(out);
    // SAFETY: helpers validate public pointer invariants.
    let value = match unsafe { lww_map_handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let encoded = match value
        .inner
        .lock()
        .map_err(|_| INTERNAL)
        .and_then(|map| map.state().map_err(|error| code(&error)))
    {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: checked by write_output.
    match unsafe { write_output(out, encoded) } {
        Ok(()) => OK,
        Err(error) => error,
    }
}

/// Writes the HLC state that must be persisted with `crdt_lww_map_state`.
///
/// # Safety
/// `value` is a live handle and `out` points to writable clock-state storage.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_clock_state(
    value: *mut CrdtLwwMap,
    out: *mut CrdtClockState,
) -> i32 {
    if out.is_null() {
        return INVALID_ARGUMENT;
    }
    // SAFETY: output pointer was checked non-null and points to writable storage by ABI contract.
    unsafe {
        *out = CrdtClockState::empty();
    }
    // SAFETY: helper validates the public pointer invariant.
    let value = match unsafe { lww_map_handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let state = match value.inner.lock().map_err(|_| INTERNAL) {
        Ok(map) => map.clock_state(),
        Err(error) => return error,
    };
    // SAFETY: output pointer remains valid for the call by ABI contract.
    unsafe {
        *out = CrdtClockState {
            replica_id: CrdtBuffer::from_vec(state.replica_id),
            wall_time: state.wall_time,
            logical: state.logical,
        };
    }
    OK
}

/// Looks up one UTF-8 key. `present` distinguishes a missing/deleted key from
/// a present key whose value is the empty byte string.
///
/// # Safety
/// `value` is a live handle; `key`, `out`, and `present` follow their pointer contracts.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_get(
    value: *mut CrdtLwwMap,
    key: *const u8,
    key_len: usize,
    out: *mut CrdtBuffer,
    present: *mut u8,
) -> i32 {
    if out.is_null() || present.is_null() {
        return INVALID_ARGUMENT;
    }
    // SAFETY: both output pointers were checked non-null and point to writable storage by ABI contract.
    unsafe {
        *out = CrdtBuffer::empty();
        *present = 0;
    }
    // SAFETY: helpers validate public pointer invariants.
    let value = match unsafe { lww_map_handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    // SAFETY: helpers validate public pointer invariants.
    let key = match unsafe { input(key, key_len) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let key = match str::from_utf8(key) {
        Ok(value) => value,
        Err(_) => return INVALID_ARGUMENT,
    };
    let result = match value.inner.lock().map_err(|_| INTERNAL) {
        Ok(map) => map.get(key),
        Err(error) => return error,
    };
    let Some(bytes) = result else { return OK };
    // SAFETY: `out` and `present` were checked above and remain writable for this call.
    unsafe {
        *out = CrdtBuffer::from_vec(bytes);
        *present = 1;
    }
    OK
}

/// Writes the visible UTF-8 keys in an FFI-only canonical list: `count` then
/// repeated length-prefixed key bytes. It is diagnostic/read API data, never a
/// CRDT frame or persistence format.
///
/// # Safety
/// `value` is a live handle and `out` points to writable buffer storage.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn crdt_lww_map_keys(value: *mut CrdtLwwMap, out: *mut CrdtBuffer) -> i32 {
    initialize_output(out);
    // SAFETY: helper validates the public pointer invariant.
    let value = match unsafe { lww_map_handle(value) } {
        Ok(value) => value,
        Err(error) => return error,
    };
    let keys = match value.inner.lock().map_err(|_| INTERNAL) {
        Ok(map) => map.keys(),
        Err(error) => return error,
    };
    let mut encoded = Vec::new();
    crate::append_uvarint(&mut encoded, keys.len() as u64);
    for key in keys {
        crate::append_bytes(&mut encoded, key.as_bytes());
    }
    // SAFETY: checked by write_output.
    match unsafe { write_output(out, encoded) } {
        Ok(()) => OK,
        Err(error) => error,
    }
}

fn initialize_output(out: *mut CrdtBuffer) {
    if !out.is_null() {
        // SAFETY: a non-null output pointer must reference writable storage by ABI contract.
        unsafe {
            *out = CrdtBuffer::empty();
        }
    }
}
