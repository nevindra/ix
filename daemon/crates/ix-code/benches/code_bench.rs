use bytes::Bytes;
use criterion::{black_box, criterion_group, criterion_main, Criterion};
use ix_code::output::extract_best_output;
use ix_code::protocol::{sign_pub, JupyterMessage};
use std::collections::HashMap;
use std::time::Duration;
use tokio::runtime::Runtime;

fn make_execute_request() -> JupyterMessage {
    JupyterMessage::new(
        "execute_request",
        "bench-session-1234",
        serde_json::json!({
            "code": "import numpy as np\nresult = np.sum(np.arange(1000))\nprint(result)",
            "silent": false,
            "store_history": true,
            "user_expressions": {},
            "allow_stdin": false,
            "stop_on_error": true
        }),
    )
}

fn bench_message_serialization(c: &mut Criterion) {
    let msg = make_execute_request();
    let key = "a-realistic-hmac-key-32-chars-xx";

    let mut group = c.benchmark_group("message_serialization");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("JupyterMessage::serialize", |b| {
        b.iter(|| {
            let frames = black_box(&msg).serialize(black_box(key));
            black_box(frames);
        });
    });

    group.finish();
}

fn bench_message_deserialization(c: &mut Criterion) {
    let msg = make_execute_request();
    let key = "a-realistic-hmac-key-32-chars-xx";
    let frames: Vec<Bytes> = msg.serialize(key);

    let mut group = c.benchmark_group("message_deserialization");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("JupyterMessage::deserialize", |b| {
        b.iter(|| {
            let result =
                JupyterMessage::deserialize(black_box(frames.clone()), black_box(key));
            black_box(result.unwrap());
        });
    });

    group.finish();
}

fn bench_hmac_computation(c: &mut Criterion) {
    let header = serde_json::to_vec(&serde_json::json!({
        "msg_id": "abc123-def456-ghi789",
        "session": "bench-session-1234",
        "username": "ix-daemon",
        "date": "2024-01-01T00:00:00+00:00",
        "msg_type": "execute_request",
        "version": "5.3"
    }))
    .unwrap();
    let parent = b"{}".to_vec();
    let metadata = b"{}".to_vec();
    let content = serde_json::to_vec(&serde_json::json!({
        "code": "import numpy as np\nresult = np.sum(np.arange(1000))\nprint(result)",
        "silent": false,
        "store_history": true,
        "user_expressions": {},
        "allow_stdin": false,
        "stop_on_error": true
    }))
    .unwrap();
    let key = "a-realistic-hmac-key-32-chars-xx";

    let mut group = c.benchmark_group("hmac_computation");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("HMAC-SHA256 sign", |b| {
        b.iter(|| {
            let sig = sign_pub(
                black_box(key),
                black_box(&[
                    header.as_slice(),
                    parent.as_slice(),
                    metadata.as_slice(),
                    content.as_slice(),
                ]),
            );
            black_box(sig);
        });
    });

    group.finish();
}

fn bench_output_extraction(c: &mut Criterion) {
    let data = serde_json::json!({
        "text/plain": "array([0, 1, 2, 3, 4])",
        "text/html": "<table><tr><td>0</td><td>1</td></tr></table>",
        "image/png": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
        "application/json": {"data": [0, 1, 2, 3, 4]}
    });

    let mut group = c.benchmark_group("output_extraction");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("extract_best_output (multi-mime)", |b| {
        b.iter(|| {
            let (rt, content) = extract_best_output(black_box(&data));
            black_box((rt, content));
        });
    });

    group.finish();
}

fn bench_output_extraction_text_only(c: &mut Criterion) {
    let data = serde_json::json!({
        "text/plain": "42"
    });

    let mut group = c.benchmark_group("output_extraction_text_only");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("extract_best_output (text only)", |b| {
        b.iter(|| {
            let result = extract_best_output(black_box(&data));
            black_box(result);
        });
    });

    group.finish();
}

fn bench_kernel_pool_grab(c: &mut Criterion) {
    let rt = Runtime::new().unwrap();

    let mut group = c.benchmark_group("kernel_pool");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("pool_grab_overhead", |b| {
        b.iter(|| {
            rt.block_on(async {
                let pool: tokio::sync::Mutex<HashMap<String, Vec<String>>> =
                    tokio::sync::Mutex::new(HashMap::new());
                {
                    let mut p = pool.lock().await;
                    p.entry("python".to_string())
                        .or_default()
                        .push("kernel-1".to_string());
                }
                let grabbed = {
                    let mut p = pool.lock().await;
                    p.get_mut("python").and_then(|v| v.pop())
                };
                black_box(grabbed);
            });
        });
    });

    group.bench_function("pool_grab_vs_cold_boot_ratio", |b| {
        b.iter(|| {
            let pool: std::sync::Mutex<HashMap<String, Vec<String>>> =
                std::sync::Mutex::new(HashMap::new());
            {
                let mut p = pool.lock().unwrap();
                p.entry("python".to_string())
                    .or_default()
                    .push("kernel-1".to_string());
            }
            let grabbed = {
                let mut p = pool.lock().unwrap();
                p.get_mut("python").and_then(|v| v.pop())
            };
            black_box(grabbed);
        });
    });

    group.finish();
}

criterion_group!(
    benches,
    bench_message_serialization,
    bench_message_deserialization,
    bench_hmac_computation,
    bench_output_extraction,
    bench_output_extraction_text_only,
    bench_kernel_pool_grab,
);
criterion_main!(benches);
