"""Safe Python ownership bindings for the DarkInno `rga-run-v2` runtime.

The package is a native Rust-backed client, not an independent Python wire
implementation. It accepts only the stable run-v2 TypeID 19/20 protocol and
keeps its resource limits inside the runtime. Configure and authenticate the
replication-group manifest before calling :meth:`RGA.apply_frame`.
"""

from __future__ import annotations

import ctypes
import os
import platform
from dataclasses import dataclass
from pathlib import Path
from typing import Final

__all__ = ["CRDTError", "ClockState", "RGA"]

_OK: Final = 0


class _Buffer(ctypes.Structure):
    _fields_ = [("data", ctypes.POINTER(ctypes.c_ubyte)), ("len", ctypes.c_size_t)]


class _ClockState(ctypes.Structure):
    _fields_ = [("replica_id", _Buffer), ("wall_time", ctypes.c_uint64), ("logical", ctypes.c_uint64)]


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
_LIBRARY.crdt_rga_new_from_clock.argtypes = [ctypes.POINTER(ctypes.c_ubyte), ctypes.c_size_t, ctypes.c_uint64, ctypes.c_uint64, ctypes.POINTER(ctypes.c_void_p)]
_LIBRARY.crdt_rga_new_from_clock.restype = ctypes.c_int32
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

    def __init__(self, replica_id: str | None = None, *, clock_state: ClockState | None = None) -> None:
        if (replica_id is None) == (clock_state is None):
            raise ValueError("provide exactly one of replica_id or clock_state")
        if clock_state is not None:
            replica_id = clock_state.replica_id
        assert replica_id is not None
        encoded = replica_id.encode("utf-8")
        pointer, _keepalive = _input(encoded)
        self._handle = ctypes.c_void_p()
        if clock_state is None:
            status = _LIBRARY.crdt_rga_new(pointer, len(encoded), ctypes.byref(self._handle))
        else:
            status = _LIBRARY.crdt_rga_new_from_clock(pointer, len(encoded), clock_state.wall_time, clock_state.logical, ctypes.byref(self._handle))
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
