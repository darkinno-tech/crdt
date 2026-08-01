use std::hint::black_box;
use std::time::Instant;

use darkinno_crdt_rga::{Limits, Rga};

fn main() {
    const ITERATIONS: u32 = 8;
    let content = benchmark_content();
    let mut total = 0_u128;
    for iteration in 0..ITERATIONS {
        let mut writer = Rga::new("writer", Limits::default()).expect("valid writer");
        let mut reader = Rga::new("reader", Limits::default()).expect("valid reader");
        let started = Instant::now();
        let delta = writer
            .insert_at(0, &content, u64::from(iteration) + 1)
            .expect("bounded local insert");
        reader
            .apply_frame_at(black_box(&delta), u64::from(iteration) + 1)
            .expect("apply writer frame");
        let state = reader.encode_state().expect("complete state");
        let mut recovered =
            Rga::new("recovered", Limits::default()).expect("valid recovery replica");
        recovered
            .apply_frame_at(black_box(&state), u64::from(iteration) + 1)
            .expect("apply state");
        black_box(recovered.text());
        total += started.elapsed().as_nanos();
    }
    eprintln!(
        "rga_run_v2_insert_replicate_recover_ns_per_op={}",
        total / u128::from(ITERATIONS)
    );

    let mut document = Rga::new("state-writer", Limits::default()).expect("valid state writer");
    document
        .insert_at(0, &content, 100)
        .expect("bounded state source insert");
    let started = Instant::now();
    for _ in 0..ITERATIONS {
        black_box(document.encode_state()).expect("bounded complete state");
    }
    eprintln!(
        "rga_run_v2_encode_complete_state_1536_ns_per_op={}",
        started.elapsed().as_nanos() / u128::from(ITERATIONS)
    );
}

fn benchmark_content() -> String {
    let mut content = "rga-run-v2 ".repeat(128);
    content.push_str(&"x".repeat(128));
    assert_eq!(content.chars().count(), 1_536);
    content
}
