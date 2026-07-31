use std::hint::black_box;
use std::time::Instant;

use darkinno_crdt_rga::{Limits, Rga};

fn main() {
    const ITERATIONS: u32 = 8;
    let mut total = 0_u128;
    for iteration in 0..ITERATIONS {
        let mut writer = Rga::new("writer", Limits::default()).expect("valid writer");
        let mut reader = Rga::new("reader", Limits::default()).expect("valid reader");
        let content = "rga-run-v2 ".repeat(128);
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
}
