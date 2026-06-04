//! End-to-end tests against a REAL pinchtab + Chrome.
//!
//! Ignored by default — they require `pinchtab` and Chrome in PATH (e.g.
//! inside the browser Docker image). pinchtab binds a fixed port (9867), so
//! these MUST run serially:
//!
//!   cargo test -p ix-browser --test browser_e2e -- --ignored --test-threads=1

use ix_browser::{BrowserBackend, PinchtabBackend};
use ix_core::types::{BrowserAction, BrowserWaitOpts};

const TEST_PAGE: &str = "data:text/html,<html><body>\
<button id='btn'>Click me</button>\
<input id='inp' type='text'/>\
<select id='sel'><option value='a'>A</option><option value='b'>B</option></select>\
<div id='hover-target'>hover</div>\
<p id='para'>hello e2e</p>\
</body></html>";

fn action(kind: &str, ref_: Option<&str>) -> BrowserAction {
    BrowserAction {
        action_type: kind.to_string(),
        element_ref: ref_.map(|s| s.to_string()),
        x: None,
        y: None,
        text: None,
        key: None,
        direction: None,
        value: None,
    }
}

/// Sweep every interaction kind the oasis `browser` tool can emit. `key` is
/// covered too: the daemon route translates it to `press` (routes/browser.rs),
/// so this exercises the post-translation set directly against pinchtab.
#[tokio::test]
#[ignore = "requires pinchtab + Chrome in PATH"]
async fn interaction_sweep_all_oasis_kinds() {
    let backend = PinchtabBackend::new().await;
    assert!(backend.available(), "pinchtab did not start — is it in PATH?");
    backend.navigate(TEST_PAGE).await.expect("navigate");

    let cases: Vec<BrowserAction> = vec![
        action("click", Some("#btn")),
        BrowserAction { text: Some("hi".into()), ..action("type", Some("#inp")) },
        BrowserAction { text: Some("hello@x.test".into()), ..action("fill", Some("#inp")) },
        BrowserAction { direction: Some("down".into()), ..action("scroll", None) },
        BrowserAction { key: Some("Enter".into()), ..action("press", None) },
        action("hover", Some("#hover-target")),
        BrowserAction { value: Some("b".into()), ..action("select", Some("#sel")) },
        action("focus", Some("#inp")),
    ];
    for case in cases {
        let kind = case.action_type.clone();
        let res = backend
            .action(case)
            .await
            .unwrap_or_else(|e| panic!("action {kind} errored: {e}"));
        assert!(res.success, "action {kind} reported failure: {:?}", res.message);
    }
    backend.shutdown().await;
}

#[tokio::test]
#[ignore = "requires pinchtab + Chrome in PATH"]
async fn wait_selector_text_load_and_timeout_smoke() {
    let backend = PinchtabBackend::new().await;
    assert!(backend.available(), "pinchtab did not start — is it in PATH?");
    backend.navigate(TEST_PAGE).await.expect("navigate");

    let wait = |kind: &str, value: Option<&str>| BrowserWaitOpts {
        kind: kind.to_string(),
        value: value.map(|s| s.to_string()),
        timeout_ms: Some(5_000),
        state: None,
    };

    let r = backend.wait(wait("selector", Some("#para"))).await.expect("wait selector");
    assert!(r.satisfied, "selector wait failed: {:?}", r.detail);

    let r = backend.wait(wait("text", Some("hello e2e"))).await.expect("wait text");
    assert!(r.satisfied, "text wait failed: {:?}", r.detail);

    let r = backend.wait(wait("load", None)).await.expect("wait load");
    assert!(r.satisfied, "load wait failed: {:?}", r.detail);

    // Timeout path: a selector that never appears → satisfied=false, NOT Err.
    let r = backend
        .wait(BrowserWaitOpts {
            kind: "selector".into(),
            value: Some("#never-exists".into()),
            timeout_ms: Some(1_000),
            state: None,
        })
        .await
        .expect("timeout wait must not error");
    assert!(!r.satisfied, "nonexistent selector must time out unsatisfied");
    assert!(r.detail.is_some(), "timeout must carry a detail message");
    assert!(r.elapsed_ms >= 1_000, "elapsed must reflect the wait");

    backend.shutdown().await;
}
