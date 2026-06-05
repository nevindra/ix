use criterion::{Criterion, criterion_group, criterion_main};
use ix_core::sse::test_channel;
use ix_core::types::ShellRequest;
use ix_shell::execute_shell;
use std::time::Duration;
use tokio::sync::mpsc;

/// Drain all SSE events from the named receiver until the channel closes.
async fn drain_events(mut rx: mpsc::Receiver<(String, String)>) -> usize {
    let mut count = 0usize;
    while rx.recv().await.is_some() {
        count += 1;
    }
    count
}

/// Benchmark: overhead of spawning a process and collecting its single-line output.
fn bench_spawn_overhead(c: &mut Criterion) {
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("failed to build tokio runtime");

    let mut group = c.benchmark_group("spawn_overhead");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("spawn_echo_hello", |b| {
        b.iter(|| {
            rt.block_on(async {
                let req = ShellRequest {
                    command: "echo hello".to_string(),
                    cwd: None,
                    timeout: None,
                    session_id: None,
                };
                let (sender, rx) = test_channel(128);
                tokio::join!(execute_shell(req, sender), drain_events(rx));
            });
        });
    });

    group.finish();
}

/// Benchmark: throughput when a command produces 1000 lines of output.
fn bench_throughput_1000_lines(c: &mut Criterion) {
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("failed to build tokio runtime");

    let mut group = c.benchmark_group("throughput");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("throughput_1000_lines", |b| {
        b.iter(|| {
            rt.block_on(async {
                let req = ShellRequest {
                    command: "for i in $(seq 1 1000); do echo \"line $i\"; done".to_string(),
                    cwd: None,
                    timeout: Some(30),
                    session_id: None,
                };
                let (sender, rx) = test_channel(1024);
                tokio::join!(execute_shell(req, sender), drain_events(rx));
            });
        });
    });

    group.finish();
}

criterion_group!(benches, bench_spawn_overhead, bench_throughput_1000_lines);
criterion_main!(benches);
