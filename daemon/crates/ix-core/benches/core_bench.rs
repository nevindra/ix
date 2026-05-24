use criterion::{black_box, criterion_group, criterion_main, Criterion, Throughput};
use ix_core::types::{
    FileContent, GrepMatch, GrepResult, ShellRequest, WebSearchResult, WebSearchResultItem,
};
use std::time::Duration;
use tokio::runtime::Runtime;

fn bench_sse(c: &mut Criterion) {
    let rt = Runtime::new().unwrap();

    let mut group = c.benchmark_group("sse");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));
    group.throughput(Throughput::Elements(1));

    group.bench_function("send_stdout", |b| {
        b.iter(|| {
            rt.block_on(async {
                let (sender, _resp) = ix_core::sse::sse_channel(1024);
                black_box(sender.send_stdout(black_box("hello from stdout")).await);
            });
        });
    });

    group.bench_function("send_complete", |b| {
        b.iter(|| {
            rt.block_on(async {
                let (sender, _resp) = ix_core::sse::sse_channel(1024);
                black_box(sender.send_complete(black_box(0), black_box(123)).await);
            });
        });
    });

    group.bench_function("send_result", |b| {
        b.iter(|| {
            rt.block_on(async {
                let (sender, _resp) = ix_core::sse::sse_channel(1024);
                black_box(
                    sender
                        .send_result(black_box("text"), black_box("some result content"))
                        .await,
                );
            });
        });
    });

    group.finish();
}

fn bench_deserialize_shell_request(c: &mut Criterion) {
    let json = r#"{"command":"cargo build --release 2>&1 | head -100","cwd":"/home/user/project","timeout":300}"#;

    let mut group = c.benchmark_group("types/deserialize");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));
    group.throughput(Throughput::Elements(1));

    group.bench_function("ShellRequest", |b| {
        b.iter(|| {
            black_box(serde_json::from_str::<ShellRequest>(black_box(json)).unwrap());
        });
    });
    group.finish();
}

fn bench_serialize_file_content(c: &mut Criterion) {
    let content =
        "use std::collections::HashMap;\n\nfn main() {\n    println!(\"hello\");\n}\n".repeat(50);
    let fc = FileContent {
        content,
        path: "/workspace/src/main.rs".to_string(),
        total_lines: 250,
    };

    let mut group = c.benchmark_group("types/serialize");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));
    group.throughput(Throughput::Elements(1));

    group.bench_function("FileContent", |b| {
        b.iter(|| {
            black_box(serde_json::to_string(black_box(&fc)).unwrap());
        });
    });
    group.finish();
}

fn bench_serialize_grep_result(c: &mut Criterion) {
    let matches: Vec<GrepMatch> = (0..20)
        .map(|i| GrepMatch {
            path: format!("src/module_{i}.rs"),
            line: i * 10 + 1,
            content: format!("fn function_{i}() -> Result<(), Error> {{"),
            context_before: vec![format!("// module {i} function"), "".to_string()],
            context_after: vec!["    todo!()".to_string(), "}".to_string()],
        })
        .collect();

    let result = GrepResult {
        matches,
        truncated: false,
    };

    let mut group = c.benchmark_group("types/serialize");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));
    group.throughput(Throughput::Elements(1));

    group.bench_function("GrepResult_20_matches", |b| {
        b.iter(|| {
            black_box(serde_json::to_string(black_box(&result)).unwrap());
        });
    });
    group.finish();
}

fn bench_serialize_web_search_result(c: &mut Criterion) {
    let result = WebSearchResult {
        query: "rust async tokio performance benchmarks".to_string(),
        results: (0..10)
            .map(|i| WebSearchResultItem {
                title: format!("Result {i}: Rust async programming guide"),
                url: format!("https://example.com/article/{i}"),
                snippet: "Learn how to write efficient async Rust code using Tokio runtime..."
                    .to_string(),
            })
            .collect(),
    };

    let mut group = c.benchmark_group("types/serialize");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));
    group.throughput(Throughput::Elements(1));

    group.bench_function("WebSearchResult_10_items", |b| {
        b.iter(|| {
            black_box(serde_json::to_string(black_box(&result)).unwrap());
        });
    });
    group.finish();
}

criterion_group!(
    benches,
    bench_sse,
    bench_deserialize_shell_request,
    bench_serialize_file_content,
    bench_serialize_grep_result,
    bench_serialize_web_search_result,
);
criterion_main!(benches);
