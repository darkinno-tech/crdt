use darkinno_crdt_rga::{Error, LwwMap, LwwMapDelta, LwwMapLimits};

const MAP_DELTA: &str = "43524454010a000e01016105616c6963650100010178dc13bbd6";
const MAP_STATE: &str = "435244540109000e01016105616c69636501000101783c3edf37";

#[test]
fn go_lww_map_vectors_decode_reencode_and_project_identically() {
    let limits = LwwMapLimits::default();
    let delta = hex_bytes(MAP_DELTA);
    assert_eq!(
        LwwMapDelta::decode(&delta, limits)
            .expect("Go delta must decode")
            .encode(limits)
            .expect("delta must re-encode canonically"),
        delta
    );

    let state = hex_bytes(MAP_STATE);
    let mut document = LwwMap::new("go-vector-reader", limits).expect("valid reader");
    document
        .apply_frame_at(&state, 100)
        .expect("Go state must decode");
    assert_eq!(document.get("a"), Some(b"x".to_vec()));
    assert_eq!(document.state().expect("canonical state"), state);
}

#[test]
fn duplicate_reordered_and_snapshot_recovery_converge() {
    let limits = LwwMapLimits::default();
    let mut alice = LwwMap::new("alice", limits).expect("alice");
    let mut bob = LwwMap::new("bob", limits).expect("bob");
    let mut carol = LwwMap::new("carol", limits).expect("carol");

    let initial = alice.set_at("title", b"draft", 1).expect("initial");
    bob.apply_frame_at(&initial, 1).expect("bob initial");
    carol.apply_frame_at(&initial, 1).expect("carol initial");
    let bob_edit = bob.set_at("owner", b"bob", 2).expect("bob edit");
    let carol_edit = carol.set_at("title", b"reviewed", 3).expect("carol edit");
    let removed = alice.delete_at("obsolete", 4).expect("delete tombstone");

    for frame in [&carol_edit, &bob_edit, &removed, &bob_edit, &initial] {
        alice.apply_frame_at(frame, 5).expect("alice delivery");
    }
    for frame in [&removed, &carol_edit, &initial, &removed] {
        bob.apply_frame_at(frame, 5).expect("bob delivery");
    }
    for frame in [&bob_edit, &removed, &bob_edit, &initial] {
        carol.apply_frame_at(frame, 5).expect("carol delivery");
    }

    assert_eq!(alice.get("title"), Some(b"reviewed".to_vec()));
    assert_eq!(alice.get("owner"), Some(b"bob".to_vec()));
    assert_eq!(alice.get("obsolete"), None);
    assert_eq!(alice.keys(), vec!["owner", "title"]);
    assert_eq!(
        alice.state().expect("alice state"),
        bob.state().expect("bob state")
    );
    assert_eq!(
        alice.state().expect("alice state"),
        carol.state().expect("carol state")
    );

    let snapshot = alice.state().expect("complete state");
    let clock = alice.clock_state();
    let mut recovered = LwwMap::from_clock_state(clock, limits).expect("restored clock");
    recovered
        .apply_frame_at(&snapshot, 6)
        .expect("snapshot recovery");
    assert_eq!(recovered.state().expect("recovered state"), snapshot);
    let next = recovered
        .set_at("after-recovery", b"safe", 6)
        .expect("post-recovery edit");
    alice
        .apply_frame_at(&next, 6)
        .expect("new tag must not conflict");
    assert_eq!(
        alice.state().expect("alice state"),
        recovered.state().expect("recovered state")
    );
}

#[test]
fn malformed_or_limited_frames_leave_map_and_clock_unchanged() {
    let limits = LwwMapLimits::default();
    let mut document = LwwMap::new("atomic", limits).expect("valid map");
    document.set_at("safe", b"value", 1).expect("seed");
    let before_state = document.state().expect("before state");
    let before_clock = document.clock_state();
    let mut corrupt = hex_bytes(MAP_DELTA);
    let last = corrupt.len() - 1;
    corrupt[last] ^= 1;

    assert_eq!(
        document.apply_frame_at(&corrupt, 2),
        Err(Error::InvalidFrame)
    );
    assert_eq!(document.state().expect("unchanged state"), before_state);
    assert_eq!(document.clock_state(), before_clock);

    let frame = hex_bytes(MAP_DELTA);
    let limited = LwwMapLimits {
        max_frame_bytes: frame.len() - 1,
        ..limits
    };
    let mut bounded = LwwMap::new("bounded", limited).expect("valid limits");
    assert_eq!(bounded.apply_frame_at(&frame, 1), Err(Error::ResourceLimit));
    assert_eq!(bounded.keys(), Vec::<String>::new());

    let entry_limits = LwwMapLimits {
        max_entries: 1,
        ..limits
    };
    let mut entry_bounded = LwwMap::new("entry-bounded", entry_limits).expect("valid limits");
    entry_bounded
        .set_at("first", b"safe", 1)
        .expect("first write");
    let before_state = entry_bounded.state().expect("before state");
    let before_clock = entry_bounded.clock_state();
    assert_eq!(
        entry_bounded.set_at("second", b"rejected", 2),
        Err(Error::ResourceLimit)
    );
    assert_eq!(
        entry_bounded.state().expect("unchanged state"),
        before_state
    );
    assert_eq!(entry_bounded.clock_state(), before_clock);
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
