use criterion::{BenchmarkId, Criterion, criterion_group, criterion_main};
use ix_fetch::fetch::extract_readable;
use ix_fetch::search::parse_results;
use std::time::Duration;

// ── realistic HTML fixtures ───────────────────────────────────────────────────

/// Generate a realistic-looking article HTML page of approximately `target_bytes`.
fn make_article_html(target_bytes: usize) -> String {
    let paragraph = "Lorem ipsum dolor sit amet, consectetur adipiscing elit. \
        Sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. \
        Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris. ";

    let mut article = String::new();
    while article.len() < target_bytes {
        article.push_str("<p>");
        article.push_str(paragraph);
        article.push_str("</p>\n");
    }

    format!(
        r#"<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>Benchmark Article Page</title>
</head>
<body>
  <header><nav><a href="/">Home</a></nav></header>
  <article>
    <h1>Main Article Heading</h1>
    {article}
  </article>
  <footer><p>Footer text</p></footer>
</body>
</html>"#
    )
}

/// Generate a Startpage-style search results page with `n` results.
fn make_search_html(n: usize) -> String {
    let mut body = String::from("<!DOCTYPE html><html><body>\n");
    for i in 1..=n {
        body.push_str(&format!(
            r#"<div class="w-gl__result">
  <h3><a href="https://example.com/result/{i}">Search Result Number {i}</a></h3>
  <p class="w-gl__result-d">This is the snippet for search result {i}. It contains a brief description.</p>
</div>
"#
        ));
    }
    body.push_str("</body></html>");
    body
}

// ── readability extraction benchmarks ────────────────────────────────────────

fn bench_extract_readable(c: &mut Criterion) {
    let html_50kb = make_article_html(50_000);
    let html_100kb = make_article_html(100_000);

    let mut group = c.benchmark_group("extract_readable");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("50kb_article", |b| {
        b.iter(|| extract_readable(&html_50kb, 8_000));
    });

    group.bench_function("100kb_article", |b| {
        b.iter(|| extract_readable(&html_100kb, 8_000));
    });

    group.bench_function("50kb_unlimited", |b| {
        b.iter(|| extract_readable(&html_50kb, usize::MAX));
    });

    group.finish();
}

// ── truncation benchmarks ─────────────────────────────────────────────────────

fn bench_truncation(c: &mut Criterion) {
    // Use a large HTML page and vary max_chars to stress the truncation path
    let html = make_article_html(60_000);

    let mut group = c.benchmark_group("truncation");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    for max_chars in [100usize, 1_000, 4_000, 8_000, 32_000] {
        group.bench_with_input(
            BenchmarkId::new("max_chars", max_chars),
            &max_chars,
            |b, &mc| {
                b.iter(|| extract_readable(&html, mc));
            },
        );
    }

    group.finish();
}

// ── search result parsing benchmarks ─────────────────────────────────────────

fn bench_parse_results(c: &mut Criterion) {
    let html_10 = make_search_html(10);
    let html_50 = make_search_html(50);

    let mut group = c.benchmark_group("parse_results");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("10_results", |b| {
        b.iter(|| parse_results(&html_10, 10));
    });

    group.bench_function("50_results_limit_10", |b| {
        b.iter(|| parse_results(&html_50, 10));
    });

    group.bench_function("50_results_unlimited", |b| {
        b.iter(|| parse_results(&html_50, 50));
    });

    group.finish();
}

criterion_group!(benches, bench_extract_readable, bench_truncation, bench_parse_results);
criterion_main!(benches);
