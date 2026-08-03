from __future__ import annotations

import os
import subprocess
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
RUST_MANIFEST = ROOT / "clients" / "rust" / "Cargo.toml"

if "CRDT_RGA_LIBRARY" not in os.environ:
    subprocess.run(["cargo", "build", "--manifest-path", str(RUST_MANIFEST)], check=True)
    os.environ["CRDT_RGA_LIBRARY"] = str(ROOT / "clients" / "rust" / "target" / "debug" / "libdarkinno_crdt_rga.dylib")

sys.path.insert(0, str(ROOT / "clients" / "python"))
from crdt_rga import CRDTError, LWWMap, LWWMapLimits, RGA, RGALimits  # noqa: E402


class RgaBindingTests(unittest.TestCase):
    def test_applies_go_canonical_vector(self) -> None:
        frame = bytes.fromhex("435244540114001201010205616c696365000100410101b20700c1d69811")
        with RGA("python-reader") as document:
            document.apply_frame(frame)
            self.assertEqual(document.text, "Aβ")
            self.assertEqual(document.state()[:6], b"CRDT\x01\x13")

    def test_three_python_replicas_converge_and_recover(self) -> None:
        with RGA("python-alice") as alice, RGA("python-bob") as bob, RGA("python-carol") as carol:
            initial = alice.insert(0, "A")
            bob.apply_frame(initial)
            carol.apply_frame(initial)
            bob_edit = bob.insert(1, "B")
            carol_edit = carol.insert(1, "C")
            for document, frames in ((alice, (carol_edit, bob_edit, bob_edit)), (bob, (carol_edit,)), (carol, (bob_edit, bob_edit))):
                for frame in frames:
                    document.apply_frame(frame)
            self.assertEqual(alice.text, bob.text)
            self.assertEqual(alice.text, carol.text)
            with RGA(clock_state=alice.clock_state) as recovered:
                recovered.apply_frame(alice.state())
                self.assertEqual(recovered.text, alice.text)
                next_delta = recovered.insert(len(recovered.text), "D")
                alice.apply_frame(next_delta)
                self.assertEqual(alice.text, recovered.text)

    def test_corrupt_frame_is_rejected_without_mutation(self) -> None:
        with RGA("python-atomic") as document:
            document.insert(0, "safe")
            before = document.text
            with self.assertRaises(CRDTError):
                document.apply_frame(b"CRDT\x01\x14\x00\x00\x00\x00\x00\x00")
            self.assertEqual(document.text, before)

    def test_recovery_retains_manifest_limits_and_rejects_atomically(self) -> None:
        limits = RGALimits(max_nodes=1, max_tags=1, max_tombstones=1)
        with RGA("python-limited", limits=limits) as limited:
            limited.insert(0, "A")
            snapshot = limited.state()
            clock = limited.clock_state

        with RGA(clock_state=clock, limits=limits) as recovered:
            with RGA("python-unbounded") as writer:
                recovered.apply_frame(snapshot)
                incoming = writer.insert(0, "B")
                before_state = recovered.state()
                before_clock = recovered.clock_state
                with self.assertRaises(CRDTError) as raised:
                    recovered.apply_frame(incoming)
                self.assertEqual(raised.exception.code, 3)
                self.assertEqual(recovered.state(), before_state)
                self.assertEqual(recovered.clock_state, before_clock)


class LwwMapBindingTests(unittest.TestCase):
    def test_applies_go_canonical_state_vector(self) -> None:
        frame = bytes.fromhex("435244540109000e01016105616c69636501000101783c3edf37")
        with LWWMap("python-map-reader") as document:
            document.apply_frame(frame)
            self.assertEqual(document.get("a"), b"x")
            self.assertEqual(document.keys(), ["a"])
            self.assertEqual(document.state(), frame)

    def test_three_replicas_converge_with_reordered_tombstone_and_recovery(self) -> None:
        with LWWMap("python-map-alice") as alice, LWWMap("python-map-bob") as bob, LWWMap("python-map-carol") as carol:
            initial = alice.set("title", b"draft")
            bob.apply_frame(initial)
            carol.apply_frame(initial)
            bob_edit = bob.set("owner", b"bob")
            carol_edit = carol.set("title", b"reviewed")
            removed = alice.delete("obsolete")
            for frame in (carol_edit, bob_edit, removed, bob_edit, initial):
                alice.apply_frame(frame)
            for frame in (removed, carol_edit, initial, removed):
                bob.apply_frame(frame)
            for frame in (bob_edit, removed, bob_edit, initial):
                carol.apply_frame(frame)
            self.assertEqual(alice.get("title"), b"reviewed")
            self.assertEqual(alice.get("owner"), b"bob")
            self.assertIsNone(alice.get("obsolete"))
            self.assertEqual(alice.keys(), ["owner", "title"])
            self.assertEqual(alice.state(), bob.state())
            self.assertEqual(alice.state(), carol.state())
            with LWWMap(clock_state=alice.clock_state) as recovered:
                recovered.apply_frame(alice.state())
                next_delta = recovered.set("after-recovery", b"safe")
                alice.apply_frame(next_delta)
                self.assertEqual(alice.state(), recovered.state())

    def test_empty_value_and_corrupt_frame_have_unambiguous_atomic_results(self) -> None:
        with LWWMap("python-map-atomic") as document:
            document.set("empty", b"")
            self.assertEqual(document.get("empty"), b"")
            before = document.state()
            corrupt = bytearray(bytes.fromhex("43524454010a000e01016105616c6963650100010178dc13bbd6"))
            corrupt[-1] ^= 1
            with self.assertRaises(CRDTError):
                document.apply_frame(bytes(corrupt))
            self.assertEqual(document.state(), before)

    def test_manifest_bound_entries_are_enforced_before_a_second_local_write(self) -> None:
        limits = LWWMapLimits(max_entries=1)
        with LWWMap("python-map-limited", limits=limits) as document:
            document.set("first", b"ok")
            before = document.state()
            with self.assertRaises(CRDTError):
                document.set("second", b"rejected")
            self.assertEqual(document.state(), before)


if __name__ == "__main__":
    unittest.main()
