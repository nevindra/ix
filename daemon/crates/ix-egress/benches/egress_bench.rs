use criterion::{black_box, criterion_group, criterion_main, BenchmarkId, Criterion};
use ix_core::types::{EgressPolicy, PolicyMode};
use ix_egress::policy::is_allowed;
use std::time::Duration;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

fn allowlist_policy(rules: Vec<String>) -> EgressPolicy {
    EgressPolicy {
        enabled: true,
        mode: PolicyMode::Allowlist,
        rules,
    }
}

fn denylist_policy(rules: Vec<String>) -> EgressPolicy {
    EgressPolicy {
        enabled: true,
        mode: PolicyMode::Denylist,
        rules,
    }
}

// ---------------------------------------------------------------------------
// Benchmark: domain matching throughput (various sizes)
// ---------------------------------------------------------------------------

fn bench_domain_matching_throughput(c: &mut Criterion) {
    let domains = vec![
        "pypi.org",
        "api.github.com",
        "registry.npmjs.org",
        "evil.com",
        "unknown-host.example",
    ];

    let rules: Vec<String> = vec![
        "pypi.org".into(),
        "*.pypi.org".into(),
        "github.com".into(),
        "*.github.com".into(),
        "registry.npmjs.org".into(),
        "api.openai.com".into(),
        "api.anthropic.com".into(),
    ];
    let policy = allowlist_policy(rules);

    let mut group = c.benchmark_group("domain_matching_throughput");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));
    for domain in &domains {
        group.bench_with_input(BenchmarkId::from_parameter(domain), domain, |b, d| {
            b.iter(|| is_allowed(black_box(d), black_box(&policy)))
        });
    }
    group.finish();
}

// ---------------------------------------------------------------------------
// Benchmark: wildcard matching at scale (100 rules, 1000 lookups)
// ---------------------------------------------------------------------------

fn bench_wildcard_at_scale(c: &mut Criterion) {
    // Build 100 wildcard rules covering different TLDs
    let rules: Vec<String> = (0..100)
        .map(|i| format!("*.rule-{i}.example.com"))
        .collect();
    let policy = allowlist_policy(rules);

    // Domain that hits the last rule (worst-case linear scan)
    let hit_domain = "sub.rule-99.example.com";
    // Domain that matches nothing (full scan, no match)
    let miss_domain = "no-match.different-tld.org";

    let mut group = c.benchmark_group("wildcard_at_scale");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("hit_last_rule", |b| {
        b.iter(|| is_allowed(black_box(hit_domain), black_box(&policy)))
    });

    group.bench_function("miss_all_rules", |b| {
        b.iter(|| is_allowed(black_box(miss_domain), black_box(&policy)))
    });

    // Simulate 1000 lookups across different domains (amortised throughput)
    let lookup_domains: Vec<String> = (0..1000)
        .map(|i| format!("sub.rule-{}.example.com", i % 100))
        .collect();

    group.bench_function("1000_lookups_amortised", |b| {
        b.iter(|| {
            for d in &lookup_domains {
                black_box(is_allowed(d, &policy));
            }
        })
    });

    group.finish();
}

// ---------------------------------------------------------------------------
// Benchmark: allowlist vs denylist mode comparison
// ---------------------------------------------------------------------------

fn bench_allowlist_vs_denylist(c: &mut Criterion) {
    let rules: Vec<String> = vec![
        "pypi.org".into(),
        "*.pypi.org".into(),
        "github.com".into(),
        "*.github.com".into(),
        "*.githubusercontent.com".into(),
        "registry.npmjs.org".into(),
        "*.npmjs.org".into(),
    ];

    let allowlist = allowlist_policy(rules.clone());
    let denylist = denylist_policy(rules);

    let domains = [
        "pypi.org",
        "api.github.com",
        "raw.githubusercontent.com",
        "evil.com",
        "unknown.example",
    ];

    let mut group = c.benchmark_group("allowlist_vs_denylist");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("allowlist_mode", |b| {
        b.iter(|| {
            for d in &domains {
                black_box(is_allowed(black_box(d), black_box(&allowlist)));
            }
        })
    });

    group.bench_function("denylist_mode", |b| {
        b.iter(|| {
            for d in &domains {
                black_box(is_allowed(black_box(d), black_box(&denylist)));
            }
        })
    });

    group.finish();
}

// ---------------------------------------------------------------------------
// Benchmark: default allowlist (empty rules → falls back to DEFAULT_ALLOWLIST)
// ---------------------------------------------------------------------------

fn bench_default_allowlist(c: &mut Criterion) {
    let policy = allowlist_policy(vec![]); // triggers DEFAULT_ALLOWLIST path

    let domains = [
        "pypi.org",
        "api.github.com",
        "files.pythonhosted.org",
        "registry.npmjs.org",
        "not-allowed.example",
    ];

    let mut group = c.benchmark_group("default_allowlist");
    group.warm_up_time(Duration::from_millis(500));
    group.measurement_time(Duration::from_secs(2));

    group.bench_function("default_allowlist_lookup", |b| {
        b.iter(|| {
            for d in &domains {
                black_box(is_allowed(black_box(d), black_box(&policy)));
            }
        })
    });

    group.finish();
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

criterion_group!(
    benches,
    bench_domain_matching_throughput,
    bench_wildcard_at_scale,
    bench_allowlist_vs_denylist,
    bench_default_allowlist,
);
criterion_main!(benches);
