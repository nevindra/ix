use criterion::{BenchmarkId, Criterion, criterion_group, criterion_main};
use ix_core::types::{EditFileRequest, GlobRequest, GrepRequest, ReadFileRequest};
use std::io::Write;
use std::time::Duration;
use tempfile::TempDir;

// ---------------------------------------------------------------------------
// Shared Tokio runtime (single-threaded, reused across all benchmarks)
// ---------------------------------------------------------------------------

fn rt() -> &'static tokio::runtime::Runtime {
    use std::sync::OnceLock;
    static RT: OnceLock<tokio::runtime::Runtime> = OnceLock::new();
    RT.get_or_init(|| {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
    })
}

// ---------------------------------------------------------------------------
// Setup helpers
// ---------------------------------------------------------------------------

/// Build a TempDir containing a single file with `n` numbered lines.
fn make_lined_file(n: usize) -> (TempDir, String) {
    let dir = TempDir::new().unwrap();
    let path = dir.path().join("bench.txt");
    let mut f = std::fs::File::create(&path).unwrap();
    for i in 1..=n {
        writeln!(f, "This is line number {}", i).unwrap();
    }
    (dir, path.to_str().unwrap().to_string())
}

/// Build a TempDir with `n` small text files, each containing a few lines.
fn make_file_dir(n: usize) -> TempDir {
    let dir = TempDir::new().unwrap();
    for i in 0..n {
        let path = dir.path().join(format!("file_{:04}.txt", i));
        let mut f = std::fs::File::create(path).unwrap();
        writeln!(f, "alpha line one").unwrap();
        writeln!(f, "NEEDLE_{} target", i).unwrap();
        writeln!(f, "gamma line three").unwrap();
    }
    dir
}

// ---------------------------------------------------------------------------
// Benchmark: File read with cat-n formatting
// ---------------------------------------------------------------------------

fn bench_read(c: &mut Criterion) {
    let mut group = c.benchmark_group("read_catn");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    for &lines in &[100usize, 1_000, 10_000] {
        let (_dir, path) = make_lined_file(lines);
        let path_clone = path.clone();

        group.bench_with_input(
            BenchmarkId::from_parameter(lines),
            &lines,
            |b, _| {
                b.iter(|| {
                    rt().block_on(async {
                        ix_files::read_file(ReadFileRequest {
                            path: path_clone.clone(),
                            offset: None,
                            limit: None,
                        })
                        .await
                        .unwrap()
                    })
                });
            },
        );
    }

    group.finish();
}

// ---------------------------------------------------------------------------
// Benchmark: Grep over a 100-file directory
// ---------------------------------------------------------------------------

fn bench_grep(c: &mut Criterion) {
    let dir = make_file_dir(100);
    let base = dir.path().to_str().unwrap().to_string();

    let mut group = c.benchmark_group("grep");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("grep_100_files", |b| {
        b.iter(|| {
            rt().block_on(async {
                ix_files::grep_files(GrepRequest {
                    pattern: "NEEDLE_".to_string(),
                    path: base.clone(),
                    glob: None,
                    context: Some(0),
                    limit: Some(200),
                })
                .await
                .unwrap()
            })
        });
    });

    group.finish();
}

// ---------------------------------------------------------------------------
// Benchmark: Glob over a 100-file directory tree
// ---------------------------------------------------------------------------

fn bench_glob(c: &mut Criterion) {
    let dir = make_file_dir(100);
    let base = dir.path().to_str().unwrap().to_string();

    let mut group = c.benchmark_group("glob");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("glob_100_files", |b| {
        b.iter(|| {
            rt().block_on(async {
                ix_files::glob_files(GlobRequest {
                    pattern: "*.txt".to_string(),
                    path: base.clone(),
                    exclude: Some(vec![".git".to_string()]),
                    limit: None,
                })
                .await
                .unwrap()
            })
        });
    });

    group.finish();
}

// ---------------------------------------------------------------------------
// Benchmark: Edit (unique replace) in a 1000-line file
// ---------------------------------------------------------------------------

fn bench_edit(c: &mut Criterion) {
    let mut group = c.benchmark_group("edit");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("edit_unique_replace_1000_lines", |b| {
        b.iter(|| {
            // Recreate the file each iteration so the target string is always present.
            let dir = TempDir::new().unwrap();
            let path = dir.path().join("edit_bench.txt");

            let mut content = String::new();
            for i in 1..=999 {
                content.push_str(&format!("line {}\n", i));
            }
            content.push_str("UNIQUE_TARGET_LINE\n");
            std::fs::write(&path, &content).unwrap();

            let path_str = path.to_str().unwrap().to_string();
            rt().block_on(async {
                ix_files::edit_file(EditFileRequest {
                    path: path_str,
                    old: "UNIQUE_TARGET_LINE".to_string(),
                    new: "REPLACED_LINE".to_string(),
                })
                .await
                .unwrap()
            });
        });
    });

    group.finish();
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

criterion_group!(benches, bench_read, bench_grep, bench_glob, bench_edit);
criterion_main!(benches);
