use darkinno_crdt_rga::{Delta, Error, Limits, Rga};

const VECTORS: &str = include_str!("../../../docs/protocol/testdata/rga-run-v2-vectors.json");

#[test]
fn go_canonical_vectors_decode_reencode_and_project_identically() {
    for (name, hex, visible) in vector_rows() {
        let frame = hex_bytes(hex);
        let mut document =
            Rga::new(format!("reader-{name}"), Limits::default()).expect("valid replica");
        document
            .apply_frame_at(&frame, 100)
            .expect("Go canonical frame must decode");
        assert_eq!(document.text(), visible, "visible projection for {name}");
        if frame[5] == 20 {
            assert_eq!(
                Delta::decode(&frame, Limits::default())
                    .expect("delta decode")
                    .encode(Limits::default())
                    .expect("delta encode"),
                frame,
                "canonical delta for {name}"
            );
        } else {
            assert_eq!(
                document.encode_state().expect("complete state"),
                frame,
                "canonical state for {name}"
            );
        }
    }
}

#[test]
fn malformed_or_limited_frames_leave_document_unchanged() {
    let (_, valid_hex, _) = vector_rows().into_iter().next().expect("first vector");
    let valid = hex_bytes(valid_hex);
    let mut document = Rga::new("atomic-reader", Limits::default()).expect("valid replica");
    document.apply_frame_at(&valid, 100).expect("valid frame");
    let before_text = document.text();
    let before_state = document.encode_state().expect("complete state");

    let mut corrupt = valid.clone();
    let last = corrupt.len() - 1;
    corrupt[last] ^= 0x01;
    assert_eq!(
        document.apply_frame_at(&corrupt, 101),
        Err(Error::InvalidFrame)
    );
    assert_eq!(document.text(), before_text);
    assert_eq!(
        document.encode_state().expect("unchanged state"),
        before_state
    );

    let limits = Limits {
        max_frame_bytes: valid.len() - 1,
        ..Limits::default()
    };
    let mut bounded = Rga::new("bounded-reader", limits).expect("valid bounds");
    assert_eq!(
        bounded.apply_frame_at(&valid, 101),
        Err(Error::ResourceLimit)
    );
    assert_eq!(bounded.text(), "");
}

#[test]
fn rejected_local_insert_leaves_clock_and_state_unchanged() {
    let limits = Limits {
        max_frame_bytes: 1,
        ..Limits::default()
    };
    let mut document = Rga::new("locally-atomic", limits).expect("valid bounds");
    let before_clock = document.clock_state();

    assert_eq!(
        document.insert_at(0, "A", 10),
        Err(Error::ResourceLimit),
        "the complete frame limit rejects the local edit"
    );
    assert_eq!(document.text(), "");
    assert_eq!(document.clock_state(), before_clock);
}

#[test]
fn duplicate_reordered_and_snapshot_recovery_converge() {
    let limits = Limits::default();
    let mut alice = Rga::new("alice", limits).expect("alice");
    let mut bob = Rga::new("bob", limits).expect("bob");
    let mut carol = Rga::new("carol", limits).expect("carol");

    let initial = alice.insert_at(0, "A", 1).expect("initial edit");
    bob.apply_frame_at(&initial, 1).expect("bob initial");
    carol.apply_frame_at(&initial, 1).expect("carol initial");
    let bob_edit = bob.insert_at(1, "B", 2).expect("bob edit");
    let carol_edit = carol.insert_at(1, "C", 3).expect("carol edit");

    for frame in [&carol_edit, &bob_edit, &bob_edit, &initial] {
        alice.apply_frame_at(frame, 4).expect("alice delivery");
    }
    for frame in [&carol_edit, &initial, &carol_edit] {
        bob.apply_frame_at(frame, 4).expect("bob delivery");
    }
    for frame in [&bob_edit, &bob_edit, &initial] {
        carol.apply_frame_at(frame, 4).expect("carol delivery");
    }

    assert_eq!(alice.text(), "ACB");
    assert_eq!(bob.text(), alice.text());
    assert_eq!(carol.text(), alice.text());

    let snapshot = alice.encode_state().expect("complete snapshot");
    let mut recovered = Rga::new("recovered", limits).expect("recovery replica");
    recovered
        .apply_frame_at(&snapshot, 5)
        .expect("state recovery");
    assert_eq!(recovered.text(), alice.text());
    assert_eq!(
        recovered.encode_state().expect("canonical recovery state"),
        snapshot
    );
}

#[test]
fn child_before_parent_is_bounded_pending_then_integrates() {
    let limits = Limits::default();
    let mut writer = Rga::new("writer", limits).expect("writer");
    let parent = writer.insert_at(0, "A", 1).expect("parent");
    let child = writer.insert_at(1, "β", 2).expect("child");
    let mut receiver = Rga::new("receiver", limits).expect("receiver");

    receiver
        .apply_frame_at(&child, 2)
        .expect("out-of-order child accepted");
    assert_eq!(receiver.pending_count(), 1);
    assert_eq!(receiver.text(), "");
    assert_eq!(receiver.encode_state(), Err(Error::IncompleteState));
    receiver
        .apply_frame_at(&parent, 2)
        .expect("parent resolves child");
    assert_eq!(receiver.pending_count(), 0);
    assert_eq!(receiver.text(), "Aβ");
}

#[test]
fn snapshot_and_clock_recovery_do_not_reuse_local_positions() {
    let limits = Limits::default();
    let mut writer = Rga::new("same-replica", limits).expect("writer");
    writer.insert_at(0, "A", 10).expect("initial edit");
    let state = writer.encode_state().expect("complete state");
    let clock = writer.clock_state();
    let mut recovered = Rga::from_clock_state(clock, limits).expect("restored clock");
    recovered
        .apply_frame_at(&state, 10)
        .expect("restored state");
    let next = recovered
        .insert_at(1, "B", 10)
        .expect("post-recovery local edit");
    writer
        .apply_frame_at(&next, 10)
        .expect("new tag must not conflict");
    assert_eq!(writer.text(), recovered.text());
}

fn vector_rows() -> Vec<(&'static str, &'static str, &'static str)> {
    const NAME: &str = "\"name\": \"";
    const HEX: &str = "\"hex\": \"";
    const VISIBLE: &str = "\"visible_text_after_apply_to_empty\": \"";
    let mut rows = Vec::new();
    let mut remaining = VECTORS;
    while let Some(name_start) = remaining.find(NAME) {
        remaining = &remaining[name_start + NAME.len()..];
        let name = remaining.split_once('"').expect("fixture name").0;
        let hex_start = remaining.find(HEX).expect("fixture hex");
        let after_hex = &remaining[hex_start + HEX.len()..];
        let hex = after_hex.split_once('"').expect("fixture hex value").0;
        let visible_start = after_hex.find(VISIBLE).expect("fixture visible text");
        let after_visible = &after_hex[visible_start + VISIBLE.len()..];
        let visible = after_visible
            .split_once('"')
            .expect("fixture visible value")
            .0;
        rows.push((name, hex, visible));
        remaining = after_visible;
    }
    rows
}

fn hex_bytes(value: &str) -> Vec<u8> {
    assert_eq!(value.len() % 2, 0, "hex has complete bytes");
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|chunk| {
            u8::from_str_radix(std::str::from_utf8(chunk).expect("ASCII hex"), 16)
                .expect("valid fixture hex")
        })
        .collect()
}
