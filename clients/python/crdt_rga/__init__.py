"""Safe Python ownership bindings for DarkInno native CRDT runtimes.

The package is a native Rust-backed client, not an independent Python wire
implementation. It exposes stable `rga-run-v2` TypeIDs 19/20 and `lww-map-v1`
TypeIDs 9/10 only; each remains independently manifest-negotiated. Configure
and authenticate the replication-group manifest before calling an apply API.
"""

from __future__ import annotations

import ctypes
import os
import platform
from dataclasses import dataclass
from pathlib import Path
from typing import Final

__all__ = ["CRDTError", "ClockState", "LWWMap", "LWWMapLimits", "RGA", "RGALimits"]

_OK: Final = 0


class _Buffer(ctypes.Structure):
    _fields_ = [("data", ctypes.POINTER(ctypes.c_ubyte)), ("len", ctypes.c_size_t)]


class _ClockState(ctypes.Structure):
    _fields_ = [("replica_id", _Buffer), ("wall_time", ctypes.c_uint64), ("logical", ctypes.c_uint64)]


class _RGALimits(ctypes.Structure):
    _fields_ = [
        ("max_frame_bytes", ctypes.c_size_t),
        ("max_payload_bytes", ctypes.c_size_t),
        ("max_string_bytes", ctypes.c_size_t),
        ("max_nodes", ctypes.c_size_t),
        ("max_tags", ctypes.c_size_t),
        ("max_tombstones", ctypes.c_size_t),
        ("max_pending_nodes", ctypes.c_size_t),
        ("max_pending_bytes", ctypes.c_size_t),
    ]


class _LWWMapLimits(ctypes.Structure):
    _fields_ = [
        ("max_frame_bytes", ctypes.c_size_t),
        ("max_payload_bytes", ctypes.c_size_t),
        ("max_string_bytes", ctypes.c_size_t),
        ("max_entries", ctypes.c_size_t),
        ("max_tombstones", ctypes.c_size_t),
    ]


def _library_path() -> Path:
    configured = os.environ.get("CRDT_RGA_LIBRARY")
    if configured:
        return Path(configured)

    root = Path(__file__).resolve().parents[3]
    suffix = {"Darwin": ".dylib", "Windows": ".dll"}.get(platform.system(), ".so")
    candidate = root / "clients" / "rust" / "target" / "debug" / f"libdarkinno_crdt_rga{suffix}"
    if candidate.is_file():
        return candidate
    raise RuntimeError(
        "CRDT_RGA_LIBRARY is not set and no development Rust library was found; "
        "build clients/rust or bundle the platform library with this package"
    )


_LIBRARY = ctypes.CDLL(str(_library_path()))
_LIBRARY.crdt_rga_new.argtypes = [ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.POINTER(ctypes.c_void_p)]
_LIBRARY.crdt_rga_new.restype = ctypes.c_int32
_LIBRARY.crdt_rga_new_with_limits.argtypes = [ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.POINTER(_RGALimits), ctypes.POINTER(ctypes.c_void_p)]
_LIBRARY.crdt_rga_new_with_limits.restype = ctypes.c_int32
_LIBRARY.crdt_rga_new_from_clock.argtypes = [ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.c_uint64, ctypes.c_uint64, ctypes.POINTER(ctypes.c_void_p)]
_LIBRARY.crdt_rga_new_from_clock.restype = ctypes.c_int32
_LIBRARY.crdt_rga_new_from_clock_with_limits.argtypes = [ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.c_uint64, ctypes.c_uint64, ctypes.POINTER(_RGALimits), ctypes.POINTER(ctypes.c_void_p)]
_LIBRARY.crdt_rga_new_from_clock_with_limits.restype = ctypes.c_int32
_LIBRARY.crdt_rga_free.argtypes = [ctypes.c_void_p]
_LIBRARY.crdt_rga_free.restype = None
_LIBRARY.crdt_rga_apply.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t]
_LIBRARY.crdt_rga_apply.restype = ctypes.c_int32
_LIBRARY.crdt_rga_insert.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.POINTER(_Buffer)]
_LIBRARY.crdt_rga_insert.restype = ctypes.c_int32
_LIBRARY.crdt_rga_delete.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_size_t, ctypes.POINTER(_Buffer)]
_LIBRARY.crdt_rga_delete.restype = ctypes.c_int32
_LIBRARY.crdt_rga_state.argtypes = [ctypes.c_void_p, ctypes.POINTER(_Buffer)]
_LIBRARY.crdt_rga_state.restype = ctypes.c_int32
_LIBRARY.crdt_rga_clock_state.argtypes = [ctypes.c_void_p, ctypes.POINTER(_ClockState)]
_LIBRARY.crdt_rga_clock_state.restype = ctypes.c_int32
_LIBRARY.crdt_rga_text.argtypes = [ctypes.c_void_p, ctypes.POINTER(_Buffer)]
_LIBRARY.crdt_rga_text.restype = ctypes.c_int32
_LIBRARY.crdt_buffer_free.argtypes = [_Buffer]
_LIBRARY.crdt_buffer_free.restype = None
_LIBRARY.crdt_clock_state_free.argtypes = [_ClockState]
_LIBRARY.crdt_clock_state_free.restype = None
_LIBRARY.crdt_lww_map_new.argtypes = [ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.POINTER(ctypes.c_void_p)]
_LIBRARY.crdt_lww_map_new.restype = ctypes.c_int32
_LIBRARY.crdt_lww_map_new_with_limits.argtypes = [ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.POINTER(_LWWMapLimits), ctypes.POINTER(ctypes.c_void_p)]
_LIBRARY.crdt_lww_map_new_with_limits.restype = ctypes.c_int32
_LIBRARY.crdt_lww_map_new_from_clock.argtypes = [ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.c_uint64, ctypes.c_uint64, ctypes.POINTER(ctypes.c_void_p)]
_LIBRARY.crdt_lww_map_new_from_clock.restype = ctypes.c_int32
_LIBRARY.crdt_lww_map_new_from_clock_with_limits.argtypes = [ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.c_uint64, ctypes.c_uint64, ctypes.POINTER(_LWWMapLimits), ctypes.POINTER(ctypes.c_void_p)]
_LIBRARY.crdt_lww_map_new_from_clock_with_limits.restype = ctypes.c_int32
_LIBRARY.crdt_lww_map_free.argtypes = [ctypes.c_void_p]
_LIBRARY.crdt_lww_map_free.restype = None
_LIBRARY.crdt_lww_map_apply.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t]
_LIBRARY.crdt_lww_map_apply.restype = ctypes.c_int32
_LIBRARY.crdt_lww_map_set.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.POINTER(_Buffer)]
_LIBRARY.crdt_lww_map_set.restype = ctypes.c_int32
_LIBRARY.crdt_lww_map_delete.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.POINTER(_Buffer)]
_LIBRARY.crdt_lww_map_delete.restype = ctypes.c_int32
_LIBRARY.crdt_lww_map_state.argtypes = [ctypes.c_void_p, ctypes.POINTER(_Buffer)]
_LIBRARY.crdt_lww_map_state.restype = ctypes.c_int32
_LIBRARY.crdt_lww_map_clock_state.argtypes = [ctypes.c_void_p, ctypes.POINTER(_ClockState)]
_LIBRARY.crdt_lww_map_clock_state.restype = ctypes.c_int32
_LIBRARY.crdt_lww_map_get.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.POINTER(_Buffer), ctypes.POINTER(ctypes.c_ubyte)]
_LIBRARY.crdt_lww_map_get.restype = ctypes.c_int32
_LIBRARY.crdt_lww_map_keys.argtypes = [ctypes.c_void_p, ctypes.POINTER(_Buffer)]
_LIBRARY.crdt_lww_map_keys.restype = ctypes.c_int32


class CRDTError(RuntimeError):
    """Native runtime rejection with its stable ABI status code."""

    def __init__(self, operation: str, code: int) -> None:
        self.operation = operation
        self.code = code
        super().__init__(f"{operation} rejected by CRDT runtime (status {code})")


@dataclass(frozen=True)
class ClockState:
    """The HLC data that must be stored atomically with ``RGA.state()``."""

    replica_id: str
    wall_time: int
    logical: int


@dataclass(frozen=True)
class RGALimits:
    """Authenticated receiver bounds for an ``RGA`` replica."""

    max_frame_bytes: int = 1 << 20
    max_payload_bytes: int = 1 << 20
    max_string_bytes: int = 64 << 10
    max_nodes: int = 100_000
    max_tags: int = 100_000
    max_tombstones: int = 100_000
    max_pending_nodes: int = 10_000
    max_pending_bytes: int = 512 << 10

    def _native(self) -> _RGALimits:
        if any(
            value <= 0
            for value in (
                self.max_frame_bytes,
                self.max_payload_bytes,
                self.max_string_bytes,
                self.max_nodes,
                self.max_tags,
                self.max_tombstones,
                self.max_pending_nodes,
                self.max_pending_bytes,
            )
        ):
            raise ValueError("all RGA limits must be positive")
        return _RGALimits(
            self.max_frame_bytes,
            self.max_payload_bytes,
            self.max_string_bytes,
            self.max_nodes,
            self.max_tags,
            self.max_tombstones,
            self.max_pending_nodes,
            self.max_pending_bytes,
        )


@dataclass(frozen=True)
class LWWMapLimits:
    """Authenticated receiver bounds for an ``LWWMap`` replica."""

    max_frame_bytes: int = 1 << 20
    max_payload_bytes: int = 1 << 20
    max_string_bytes: int = 64 << 10
    max_entries: int = 100_000
    max_tombstones: int = 100_000

    def _native(self) -> _LWWMapLimits:
        if any(
            value <= 0
            for value in (
                self.max_frame_bytes,
                self.max_payload_bytes,
                self.max_string_bytes,
                self.max_entries,
                self.max_tombstones,
            )
        ):
            raise ValueError("all LWWMap limits must be positive")
        return _LWWMapLimits(
            self.max_frame_bytes,
            self.max_payload_bytes,
            self.max_string_bytes,
            self.max_entries,
            self.max_tombstones,
        )


def _input(value: bytes) -> tuple[ctypes.POINTER(ctypes.c_ubyte) | None, object | None]:
    if not value:
        return None, None
    buffer = (ctypes.c_ubyte * len(value)).from_buffer_copy(value)
    return buffer, buffer


def _check(operation: str, code: int) -> None:
    if code != _OK:
        raise CRDTError(operation, code)


class RGA:
    """A local, bounded collaborative-text replica backed by Rust.

    Offsets count Unicode scalar values, never UTF-8 bytes. ``insert`` and
    ``delete`` return already-applied canonical run-v2 delta frames; callers
    relay those frames to separately authenticated peers. ``state`` and the
    clock/outbox position must be persisted atomically before replica-ID reuse.
    """

    def __init__(
        self,
        replica_id: str | None = None,
        *,
        clock_state: ClockState | None = None,
        limits: RGALimits | None = None,
    ) -> None:
        if (replica_id is None) == (clock_state is None):
            raise ValueError("provide exactly one of replica_id or clock_state")
        if clock_state is not None:
            replica_id = clock_state.replica_id
        assert replica_id is not None
        encoded = replica_id.encode("utf-8")
        pointer, _keepalive = _input(encoded)
        self._handle = ctypes.c_void_p()
        native_limits = limits._native() if limits is not None else None
        if clock_state is None:
            status = (
                _LIBRARY.crdt_rga_new(pointer, len(encoded), ctypes.byref(self._handle))
                if native_limits is None
                else _LIBRARY.crdt_rga_new_with_limits(
                    pointer,
                    len(encoded),
                    ctypes.byref(native_limits),
                    ctypes.byref(self._handle),
                )
            )
        else:
            status = (
                _LIBRARY.crdt_rga_new_from_clock(
                    pointer,
                    len(encoded),
                    clock_state.wall_time,
                    clock_state.logical,
                    ctypes.byref(self._handle),
                )
                if native_limits is None
                else _LIBRARY.crdt_rga_new_from_clock_with_limits(
                    pointer,
                    len(encoded),
                    clock_state.wall_time,
                    clock_state.logical,
                    ctypes.byref(native_limits),
                    ctypes.byref(self._handle),
                )
            )
        _check("new", status)

    def close(self) -> None:
        """Release native state. The object cannot be used afterward."""
        if self._handle is not None:
            _LIBRARY.crdt_rga_free(self._handle)
            self._handle = None

    def __enter__(self) -> RGA:
        return self

    def __exit__(self, _type: object, _value: object, _traceback: object) -> None:
        self.close()

    def __del__(self) -> None:
        self.close()

    def apply_frame(self, frame: bytes) -> None:
        """Atomically apply one authenticated run-v2 state or delta frame."""
        handle = self._require_handle()
        pointer, _keepalive = _input(frame)
        _check("apply_frame", _LIBRARY.crdt_rga_apply(handle, pointer, len(frame)))

    def insert(self, offset: int, value: str) -> bytes:
        """Insert text before ``offset`` and return the applied delta frame."""
        if offset < 0:
            raise ValueError("offset must be non-negative")
        encoded = value.encode("utf-8")
        pointer, _keepalive = _input(encoded)
        return self._output("insert", _LIBRARY.crdt_rga_insert, self._require_handle(), offset, pointer, len(encoded))

    def delete(self, offset: int, count: int) -> bytes:
        """Tombstone visible scalars and return the applied delta frame."""
        if offset < 0 or count < 0:
            raise ValueError("offset and count must be non-negative")
        return self._output("delete", _LIBRARY.crdt_rga_delete, self._require_handle(), offset, count)

    def state(self) -> bytes:
        """Return a complete state frame, or reject while parents are pending."""
        return self._output("state", _LIBRARY.crdt_rga_state, self._require_handle())

    @property
    def clock_state(self) -> ClockState:
        """Return the HLC state that must be persisted with ``state()``."""
        output = _ClockState()
        _check("clock_state", _LIBRARY.crdt_rga_clock_state(self._require_handle(), ctypes.byref(output)))
        try:
            replica_id = ctypes.string_at(output.replica_id.data, output.replica_id.len).decode("utf-8")
            return ClockState(replica_id, output.wall_time, output.logical)
        finally:
            _LIBRARY.crdt_clock_state_free(output)

    @property
    def text(self) -> str:
        """Return the deterministic visible text projection."""
        return self._output("text", _LIBRARY.crdt_rga_text, self._require_handle()).decode("utf-8")

    def _require_handle(self) -> ctypes.c_void_p:
        if self._handle is None:
            raise RuntimeError("RGA is closed")
        return self._handle

    @staticmethod
    def _output(operation: str, function: object, *arguments: object) -> bytes:
        output = _Buffer()
        _check(operation, function(*arguments, ctypes.byref(output)))
        try:
            return ctypes.string_at(output.data, output.len)
        finally:
            _LIBRARY.crdt_buffer_free(output)


class LWWMap:
    """A local byte-value LWW-Map v1 replica backed by Rust.

    ``set`` and ``delete`` return already-applied canonical TypeID 10 frames.
    ``state`` returns TypeID 9. Map values are opaque bytes: bind their schema,
    limits, document, epoch, and authorization in the authenticated manifest.
    ``state`` and ``clock_state`` must be persisted atomically with the
    application frontier/outbox before reuse of ``replica_id``.
    """

    def __init__(self, replica_id: str | None = None, *, clock_state: ClockState | None = None, limits: LWWMapLimits | None = None) -> None:
        if (replica_id is None) == (clock_state is None):
            raise ValueError("provide exactly one of replica_id or clock_state")
        if clock_state is not None:
            replica_id = clock_state.replica_id
        assert replica_id is not None
        encoded = replica_id.encode("utf-8")
        pointer, _keepalive = _input(encoded)
        self._handle = ctypes.c_void_p()
        native_limits = limits._native() if limits is not None else None
        if clock_state is None:
            status = (
                _LIBRARY.crdt_lww_map_new(pointer, len(encoded), ctypes.byref(self._handle))
                if native_limits is None
                else _LIBRARY.crdt_lww_map_new_with_limits(
                    pointer,
                    len(encoded),
                    ctypes.byref(native_limits),
                    ctypes.byref(self._handle),
                )
            )
        else:
            status = (
                _LIBRARY.crdt_lww_map_new_from_clock(
                    pointer,
                    len(encoded),
                    clock_state.wall_time,
                    clock_state.logical,
                    ctypes.byref(self._handle),
                )
                if native_limits is None
                else _LIBRARY.crdt_lww_map_new_from_clock_with_limits(
                    pointer,
                    len(encoded),
                    clock_state.wall_time,
                    clock_state.logical,
                    ctypes.byref(native_limits),
                    ctypes.byref(self._handle),
                )
            )
        _check("lww_map.new", status)

    def close(self) -> None:
        """Release native state. The object cannot be used afterward."""
        if self._handle is not None:
            _LIBRARY.crdt_lww_map_free(self._handle)
            self._handle = None

    def __enter__(self) -> LWWMap:
        return self

    def __exit__(self, _type: object, _value: object, _traceback: object) -> None:
        self.close()

    def __del__(self) -> None:
        self.close()

    def apply_frame(self, frame: bytes) -> None:
        """Atomically apply one authenticated LWW-Map TypeID 9 or 10 frame."""
        pointer, _keepalive = _input(frame)
        _check("lww_map.apply_frame", _LIBRARY.crdt_lww_map_apply(self._require_handle(), pointer, len(frame)))

    def set(self, key: str, value: bytes) -> bytes:
        """Set one UTF-8 key and return its already-applied canonical delta."""
        key_bytes = key.encode("utf-8")
        key_pointer, _key_keepalive = _input(key_bytes)
        value_pointer, _value_keepalive = _input(value)
        return self._output(
            "lww_map.set",
            _LIBRARY.crdt_lww_map_set,
            self._require_handle(),
            key_pointer,
            len(key_bytes),
            value_pointer,
            len(value),
        )

    def delete(self, key: str) -> bytes:
        """Delete one UTF-8 key and return its already-applied canonical delta."""
        key_bytes = key.encode("utf-8")
        key_pointer, _key_keepalive = _input(key_bytes)
        return self._output(
            "lww_map.delete",
            _LIBRARY.crdt_lww_map_delete,
            self._require_handle(),
            key_pointer,
            len(key_bytes),
        )

    def get(self, key: str) -> bytes | None:
        """Return a visible opaque value, including ``b\"\"``, or ``None``."""
        key_bytes = key.encode("utf-8")
        key_pointer, _key_keepalive = _input(key_bytes)
        output = _Buffer()
        present = ctypes.c_ubyte()
        _check(
            "lww_map.get",
            _LIBRARY.crdt_lww_map_get(
                self._require_handle(),
                key_pointer,
                len(key_bytes),
                ctypes.byref(output),
                ctypes.byref(present),
            ),
        )
        try:
            return ctypes.string_at(output.data, output.len) if present.value else None
        finally:
            _LIBRARY.crdt_buffer_free(output)

    def keys(self) -> list[str]:
        """Return visible keys in canonical order; this list is not a CRDT frame."""
        payload = self._output("lww_map.keys", _LIBRARY.crdt_lww_map_keys, self._require_handle())
        cursor = 0
        count, cursor = _read_uvarint(payload, cursor)
        keys: list[str] = []
        for _ in range(count):
            size, cursor = _read_uvarint(payload, cursor)
            end = cursor + size
            if end > len(payload):
                raise RuntimeError("native LWWMap returned an invalid key list")
            keys.append(payload[cursor:end].decode("utf-8"))
            cursor = end
        if cursor != len(payload):
            raise RuntimeError("native LWWMap returned trailing key-list bytes")
        return keys

    def state(self) -> bytes:
        """Return a complete canonical LWW-Map TypeID 9 state frame."""
        return self._output("lww_map.state", _LIBRARY.crdt_lww_map_state, self._require_handle())

    @property
    def clock_state(self) -> ClockState:
        """Return the HLC state that must be persisted with ``state()``."""
        output = _ClockState()
        _check("lww_map.clock_state", _LIBRARY.crdt_lww_map_clock_state(self._require_handle(), ctypes.byref(output)))
        try:
            replica_id = ctypes.string_at(output.replica_id.data, output.replica_id.len).decode("utf-8")
            return ClockState(replica_id, output.wall_time, output.logical)
        finally:
            _LIBRARY.crdt_clock_state_free(output)

    def _require_handle(self) -> ctypes.c_void_p:
        if self._handle is None:
            raise RuntimeError("LWWMap is closed")
        return self._handle

    @staticmethod
    def _output(operation: str, function: object, *arguments: object) -> bytes:
        output = _Buffer()
        _check(operation, function(*arguments, ctypes.byref(output)))
        try:
            return ctypes.string_at(output.data, output.len)
        finally:
            _LIBRARY.crdt_buffer_free(output)


def _read_uvarint(payload: bytes, cursor: int) -> tuple[int, int]:
    start = cursor
    value = 0
    for shift in range(10):
        if cursor >= len(payload):
            raise RuntimeError("native LWWMap returned a truncated key list")
        byte = payload[cursor]
        cursor += 1
        if shift == 9 and byte > 1:
            raise RuntimeError("native LWWMap returned an overflowing key list")
        value |= (byte & 0x7F) << (shift * 7)
        if not byte & 0x80:
            if cursor - start != _uvarint_size(value):
                raise RuntimeError("native LWWMap returned a non-canonical key list")
            return value, cursor
    raise RuntimeError("native LWWMap returned an overflowing key list")


def _uvarint_size(value: int) -> int:
    size = 1
    while value >= 0x80:
        value >>= 7
        size += 1
    return size
