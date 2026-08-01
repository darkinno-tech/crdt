import RGAFormal.Delta

/-!
# run-v2 envelope refinement model

This is deliberately an envelope model, not a parser proof.  It states the
admission boundary shared by the run-v2 specification: one canonical version,
the 19/20 type pair, an empty codec ID, and a checksum over the admitted body.
The byte parser, varints, CRC-32C implementation, resource limits, and graph
decoder remain concrete obligations for Go tests and fuzzing.
-/

namespace DarkInno.RGA.RunV2

inductive FrameKind where
  | state
  | delta
  deriving DecidableEq, Repr

def kindTag : FrameKind → Nat
  | .state => 19
  | .delta => 20

def parseKind : Nat → Option FrameKind
  | 19 => some .state
  | 20 => some .delta
  | _ => none

/- The checksum is abstracted to a deterministic function.  Replacing this
with CRC-32C is a parser/refinement obligation, not an algebraic shortcut. -/
def checksum (kind : Nat) (codec payload : List Nat) : Nat :=
  (codec ++ payload).foldl (fun acc byte => acc * 257 + byte) kind

structure Frame where
  kind : FrameKind
  payload : List Nat
  deriving DecidableEq, Repr

/- `Wire` is the parsed envelope before admission.  Byte lengths and varints
are intentionally below this refinement boundary. -/
structure Wire where
  version : Nat
  typeTag : Nat
  codec : List Nat
  payload : List Nat
  checksum : Nat
  deriving DecidableEq, Repr

def encode (frame : Frame) : Wire :=
  { version := 1
    typeTag := kindTag frame.kind
    codec := []
    payload := frame.payload
    checksum := checksum (kindTag frame.kind) [] frame.payload }

/- `decode` admits only the one negotiated run-v2 envelope. -/
def decode (wire : Wire) : Option Frame := do
  if wire.version != 1 then none else
  let kind ← parseKind wire.typeTag
  if wire.codec != [] then none else
  if wire.checksum != checksum wire.typeTag wire.codec wire.payload then none else
  some { kind, payload := wire.payload }

/- A canonical encoder always refines to the exact abstract frame it started
from. -/
theorem decode_encode (frame : Frame) : decode (encode frame) = some frame := by
  cases frame with
  | mk kind payload =>
    cases kind <;> simp [decode, encode, parseKind, kindTag]

/- No frame outside the selected version refines to a run-v2 frame. -/
theorem wrong_version_rejected (wire : Wire) (invalid : wire.version ≠ 1) :
    decode wire = none := by
  simp [decode, invalid]

/- A non-empty codec ID cannot be smuggled into the fixed empty-codec group. -/
theorem nonempty_codec_rejected (wire : Wire) (version : wire.version = 1)
    (codec : wire.codec ≠ []) : decode wire = none := by
  simp [decode, version, codec]

/- Any checksum mismatch is rejected before a frame is exposed to semantic
application. -/
theorem checksum_mismatch_rejected (wire : Wire) (version : wire.version = 1)
    (codec : wire.codec = [])
    (mismatch : wire.checksum ≠ checksum wire.typeTag wire.codec wire.payload) :
    decode wire = none := by
  have normalizedMismatch : wire.checksum ≠ checksum wire.typeTag [] wire.payload := by
    simpa [codec] using mismatch
  simp [decode, version, codec, normalizedMismatch]

/- The two run-v2 type tags are disjoint, so a state envelope cannot be
silently interpreted as a delta envelope (or conversely). -/
theorem state_delta_type_tags_distinct : kindTag .state ≠ kindTag .delta := by
  decide

end DarkInno.RGA.RunV2
