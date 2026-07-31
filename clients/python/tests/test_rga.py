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
from crdt_rga import CRDTError, RGA  # noqa: E402


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


if __name__ == "__main__":
    unittest.main()
