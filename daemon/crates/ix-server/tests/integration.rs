/// Integration tests for ix-server routes.
///
/// Each test builds the full Axum router with a minimal test AppState, then
/// fires requests through `tower::ServiceExt::oneshot`.
use std::sync::Arc;

use async_trait::async_trait;
use axum::body::Body;
use axum::http::{Request, StatusCode};
use http_body_util::BodyExt;
use ix_browser::BrowserBackend;
use ix_core::types::{
    BrowserAction, BrowserFindResult, BrowserResult, BrowserSnapshot, BrowserTextResult,
    NavigateResult, SnapshotOpts, TextOpts,
};
use ix_core::Result;
use ix_server::router::build_router;
use ix_server::state::AppState;
use tower::ServiceExt;

// ─── Mock browser backend ──────────────────────────────────────────────────────

struct MockBrowser;

#[async_trait]
impl BrowserBackend for MockBrowser {
    fn available(&self) -> bool {
        false
    }
    async fn navigate(&self, _url: &str) -> Result<NavigateResult> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn screenshot(&self) -> Result<Vec<u8>> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn action(&self, _action: BrowserAction) -> Result<BrowserResult> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn snapshot(&self, _opts: SnapshotOpts) -> Result<BrowserSnapshot> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn text(&self, _opts: TextOpts) -> Result<BrowserTextResult> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn pdf(&self) -> Result<Vec<u8>> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn eval(&self, _expr: &str) -> Result<String> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
    async fn find(&self, _query: &str) -> Result<BrowserFindResult> {
        Err(ix_core::Error::Unavailable("mock".into()))
    }
}

// ─── Test helpers ──────────────────────────────────────────────────────────────

/// Build a test AppState with the given workspace directory.
fn make_state(workspace: &str) -> Arc<AppState> {
    let config = ix_core::config::DaemonConfig {
        addr: "127.0.0.1:0".to_string(),
        workspace: workspace.to_string(),
        egress: ix_core::types::EgressPolicy::default(),
    };
    Arc::new(AppState {
        config,
        browser: Arc::new(MockBrowser),
        kernels: Arc::new(ix_code::KernelManager::new()),
        egress: None,
        start_time: std::time::Instant::now(),
    })
}

/// Drain the response body into bytes.
async fn body_bytes(body: Body) -> bytes::Bytes {
    body.collect().await.unwrap().to_bytes()
}

/// Drain the response body and parse it as JSON.
async fn body_json(body: Body) -> serde_json::Value {
    let bytes = body_bytes(body).await;
    serde_json::from_slice(&bytes).expect("response was not valid JSON")
}

// ─── Health ────────────────────────────────────────────────────────────────────

#[tokio::test]
async fn health_returns_200_with_ok_status() {
    let state = make_state("/tmp");
    let app = build_router(state);

    let resp = app
        .oneshot(
            Request::builder()
                .uri("/health")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    assert_eq!(json["status"], "ok");
    assert!(json["uptime_sec"].is_number());
    assert_eq!(json["browser"], false);
}

// ─── Request-ID middleware ─────────────────────────────────────────────────────

#[tokio::test]
async fn response_has_x_request_id_header() {
    let state = make_state("/tmp");
    let app = build_router(state);

    let resp = app
        .oneshot(
            Request::builder()
                .uri("/health")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();

    assert!(
        resp.headers().contains_key("x-request-id"),
        "missing X-Request-Id header"
    );
}

#[tokio::test]
async fn x_request_id_is_valid_uuid() {
    let state = make_state("/tmp");
    let app = build_router(state);

    let resp = app
        .oneshot(
            Request::builder()
                .uri("/health")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();

    let id = resp
        .headers()
        .get("x-request-id")
        .expect("missing X-Request-Id header")
        .to_str()
        .unwrap();

    uuid::Uuid::parse_str(id).expect("X-Request-Id is not a valid UUID");
}

// ─── File endpoints ────────────────────────────────────────────────────────────

#[tokio::test]
async fn write_then_read_returns_correct_content() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("hello.txt");
    let path_str = path.to_str().unwrap();

    let state = make_state(dir.path().to_str().unwrap());
    let app = build_router(state);

    // Write
    let write_body = serde_json::json!({
        "path": path_str,
        "content": "hello world\n"
    })
    .to_string();

    let resp = app
        .clone()
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/file/write")
                .header("content-type", "application/json")
                .body(Body::from(write_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    assert!(json["bytes_written"].as_u64().unwrap() > 0);

    // Read
    let read_body = serde_json::json!({ "path": path_str }).to_string();
    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/file/read")
                .header("content-type", "application/json")
                .body(Body::from(read_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    // read_file returns content in cat-n format: "     1\thello world"
    let content = json["content"].as_str().unwrap();
    assert!(
        content.contains("hello world"),
        "expected 'hello world' in content, got: {content}"
    );
    assert!(
        content.contains("\t"),
        "expected cat-n tab delimiter in content"
    );
}

#[tokio::test]
async fn edit_file_with_unique_match_succeeds() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("edit_me.txt");
    let path_str = path.to_str().unwrap();

    // Create file first
    std::fs::write(&path, "foo bar baz\n").unwrap();

    let state = make_state(dir.path().to_str().unwrap());
    let app = build_router(state);

    let edit_body = serde_json::json!({
        "path": path_str,
        "old": "foo bar",
        "new": "replaced"
    })
    .to_string();

    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/file/edit")
                .header("content-type", "application/json")
                .body(Body::from(edit_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    assert_eq!(json["applied"], true);

    // Verify content
    let contents = std::fs::read_to_string(&path).unwrap();
    assert!(contents.contains("replaced"));
    assert!(!contents.contains("foo bar"));
}

#[tokio::test]
async fn edit_file_with_nonexistent_match_returns_error() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("no_match.txt");
    let path_str = path.to_str().unwrap();

    std::fs::write(&path, "actual content\n").unwrap();

    let state = make_state(dir.path().to_str().unwrap());
    let app = build_router(state);

    let edit_body = serde_json::json!({
        "path": path_str,
        "old": "this string does not exist",
        "new": "replacement"
    })
    .to_string();

    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/file/edit")
                .header("content-type", "application/json")
                .body(Body::from(edit_body))
                .unwrap(),
        )
        .await
        .unwrap();

    assert!(
        resp.status().is_client_error() || resp.status().is_server_error(),
        "expected error status, got {}",
        resp.status()
    );
}

#[tokio::test]
async fn glob_returns_matching_files() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("a.txt"), "a").unwrap();
    std::fs::write(dir.path().join("b.txt"), "b").unwrap();
    std::fs::write(dir.path().join("c.rs"), "c").unwrap();

    let state = make_state(dir.path().to_str().unwrap());
    let app = build_router(state);

    let glob_body = serde_json::json!({
        "pattern": "**/*.txt",
        "path": dir.path().to_str().unwrap()
    })
    .to_string();

    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/file/glob")
                .header("content-type", "application/json")
                .body(Body::from(glob_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    let files = json["files"].as_array().unwrap();
    assert_eq!(files.len(), 2, "expected 2 .txt files");
    let file_names: Vec<&str> = files.iter().map(|f| f.as_str().unwrap()).collect();
    assert!(file_names.iter().any(|f| f.ends_with(".txt")));
    assert!(!file_names.iter().any(|f| f.ends_with(".rs")));
}

#[tokio::test]
async fn grep_returns_matches() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("search_me.txt"), "needle in a haystack\nno match here\n")
        .unwrap();

    let state = make_state(dir.path().to_str().unwrap());
    let app = build_router(state);

    let grep_body = serde_json::json!({
        "pattern": "needle",
        "path": dir.path().to_str().unwrap()
    })
    .to_string();

    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/file/grep")
                .header("content-type", "application/json")
                .body(Body::from(grep_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    let matches = json["matches"].as_array().unwrap();
    assert!(!matches.is_empty(), "expected at least one match");
    assert!(matches[0]["content"].as_str().unwrap().contains("needle"));
}

#[tokio::test]
async fn tree_returns_tree_string() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("f1.txt"), "a").unwrap();
    std::fs::create_dir(dir.path().join("subdir")).unwrap();
    std::fs::write(dir.path().join("subdir").join("f2.txt"), "b").unwrap();

    let state = make_state(dir.path().to_str().unwrap());
    let app = build_router(state);

    let tree_body = serde_json::json!({
        "path": dir.path().to_str().unwrap()
    })
    .to_string();

    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/file/tree")
                .header("content-type", "application/json")
                .body(Body::from(tree_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    assert!(json["tree"].is_string());
    assert!(json["files"].is_number());
    assert!(json["dirs"].is_number());
}

#[tokio::test]
async fn stat_file_returns_metadata() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("stat_me.txt");
    std::fs::write(&path, "some content\n").unwrap();

    let state = make_state(dir.path().to_str().unwrap());
    let app = build_router(state);

    let uri = format!("/v1/file/stat?path={}", path.to_str().unwrap());
    let resp = app
        .oneshot(
            Request::builder()
                .method("GET")
                .uri(&uri)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    // FileStat should have size and other metadata
    assert!(json["size"].is_number());
}

#[tokio::test]
async fn list_dir_returns_entries() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("entry1.txt"), "a").unwrap();
    std::fs::write(dir.path().join("entry2.txt"), "b").unwrap();

    let state = make_state(dir.path().to_str().unwrap());
    let app = build_router(state);

    let ls_body = serde_json::json!({
        "path": dir.path().to_str().unwrap()
    })
    .to_string();

    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/file/ls")
                .header("content-type", "application/json")
                .body(Body::from(ls_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    let entries = json["entries"].as_array().unwrap();
    assert_eq!(entries.len(), 2);
}

#[tokio::test]
async fn upload_file_writes_content() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("uploaded.txt");
    let path_str = path.to_str().unwrap();

    let state = make_state(dir.path().to_str().unwrap());
    let app = build_router(state);

    // Build multipart body manually
    let boundary = "----TestBoundary1234";
    let multipart_body = format!(
        "--{boundary}\r\nContent-Disposition: form-data; name=\"path\"\r\n\r\n{path_str}\r\n\
         --{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"uploaded.txt\"\r\n\
         Content-Type: text/plain\r\n\r\nuploaded content\r\n\
         --{boundary}--\r\n"
    );

    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/v1/file/upload")
                .header(
                    "content-type",
                    format!("multipart/form-data; boundary={boundary}"),
                )
                .body(Body::from(multipart_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    assert!(json["bytes_written"].as_u64().unwrap() > 0);
    assert_eq!(std::fs::read_to_string(&path).unwrap(), "uploaded content");
}

#[tokio::test]
async fn download_file_returns_content() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("download_me.txt");
    std::fs::write(&path, "download content").unwrap();

    let state = make_state(dir.path().to_str().unwrap());
    let app = build_router(state);

    let uri = format!("/v1/file/download?path={}", path.to_str().unwrap());
    let resp = app
        .oneshot(
            Request::builder()
                .method("GET")
                .uri(&uri)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let body = body_bytes(resp.into_body()).await;
    assert_eq!(body.as_ref(), b"download content");
}

// ─── Egress endpoints ──────────────────────────────────────────────────────────

#[tokio::test]
async fn egress_get_policy_returns_default_when_no_filter() {
    let state = make_state("/tmp");
    let app = build_router(state);

    let resp = app
        .oneshot(
            Request::builder()
                .method("GET")
                .uri("/v1/egress/policy")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    assert_eq!(json["enabled"], false);
    assert!(json["rules"].as_array().unwrap().is_empty());
}

#[tokio::test]
async fn egress_patch_policy_returns_503_when_no_filter() {
    let state = make_state("/tmp");
    let app = build_router(state);

    let patch_body = serde_json::json!({ "add": ["example.com"] }).to_string();

    let resp = app
        .oneshot(
            Request::builder()
                .method("PATCH")
                .uri("/v1/egress/policy")
                .header("content-type", "application/json")
                .body(Body::from(patch_body))
                .unwrap(),
        )
        .await
        .unwrap();
    // Without an egress filter in state, patch should return 503 Unavailable
    assert_eq!(resp.status(), StatusCode::SERVICE_UNAVAILABLE);
}

// ─── Workspace endpoint ────────────────────────────────────────────────────────

#[tokio::test]
async fn workspace_info_returns_os_arch_and_working_dir() {
    let dir = tempfile::tempdir().unwrap();
    let workspace = dir.path().to_str().unwrap().to_string();

    let state = make_state(&workspace);
    let app = build_router(state);

    let resp = app
        .oneshot(
            Request::builder()
                .method("GET")
                .uri("/v1/workspace/info")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let json = body_json(resp.into_body()).await;
    assert!(json["os"].is_string());
    assert!(json["arch"].is_string());
    assert_eq!(json["working_dir"], workspace);
    assert_eq!(json["browser"], false);
}

// ─── E2B route compatibility ───────────────────────────────────────────────────

#[tokio::test]
async fn e2b_health_endpoint_returns_200() {
    let state = make_state("/tmp");
    let app = build_router(state);

    let resp = app
        .oneshot(
            Request::builder()
                .uri("/health")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
}

#[tokio::test]
async fn e2b_commands_run_accepts_sandbox_id_path_param() {
    let state = make_state("/tmp");
    let app = build_router(state);

    // This route should be recognized (not 404). The handler may respond with
    // any non-404 status since shell execution isn't fully tested here.
    let body = serde_json::json!({ "command": "true" }).to_string();
    let resp = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/sandboxes/test-sandbox-id/commands/run")
                .header("content-type", "application/json")
                .body(Body::from(body))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_ne!(
        resp.status(),
        StatusCode::NOT_FOUND,
        "E2B route was not registered"
    );
}

#[tokio::test]
async fn e2b_files_list_accepts_sandbox_id() {
    let dir = tempfile::tempdir().unwrap();
    let path_str = dir.path().to_str().unwrap();

    let state = make_state(path_str);
    let app = build_router(state);

    let uri = format!("/sandboxes/abc-123/files/list?path={path_str}");
    let resp = app
        .oneshot(
            Request::builder()
                .method("GET")
                .uri(&uri)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();

    assert_ne!(resp.status(), StatusCode::NOT_FOUND);
    assert_eq!(resp.status(), StatusCode::OK);
}

// ─── Browser endpoints return 503 when unavailable ────────────────────────────

async fn browser_route_returns_503(method: &str, uri: &str, json_body: Option<serde_json::Value>) {
    let state = make_state("/tmp");
    let app = build_router(state);

    let mut builder = Request::builder().method(method).uri(uri);

    let body = if let Some(json) = json_body {
        builder = builder.header("content-type", "application/json");
        Body::from(json.to_string())
    } else {
        Body::empty()
    };

    let resp = app.oneshot(builder.body(body).unwrap()).await.unwrap();
    assert_eq!(
        resp.status(),
        StatusCode::SERVICE_UNAVAILABLE,
        "expected 503 for {method} {uri}, got {}",
        resp.status()
    );
}

#[tokio::test]
async fn browser_navigate_returns_503_when_unavailable() {
    browser_route_returns_503(
        "POST",
        "/v1/browser/navigate",
        Some(serde_json::json!({ "url": "https://example.com" })),
    )
    .await;
}

#[tokio::test]
async fn browser_screenshot_returns_503_when_unavailable() {
    browser_route_returns_503("GET", "/v1/browser/screenshot", None).await;
}

#[tokio::test]
async fn browser_action_returns_503_when_unavailable() {
    browser_route_returns_503(
        "POST",
        "/v1/browser/action",
        Some(serde_json::json!({
            "kind": "click",
            "ref": null,
            "x": null,
            "y": null,
            "text": null,
            "key": null,
            "direction": null,
            "value": null
        })),
    )
    .await;
}

#[tokio::test]
async fn browser_snapshot_returns_503_when_unavailable() {
    browser_route_returns_503("GET", "/v1/browser/snapshot", None).await;
}

#[tokio::test]
async fn browser_text_returns_503_when_unavailable() {
    browser_route_returns_503("GET", "/v1/browser/text", None).await;
}

#[tokio::test]
async fn browser_pdf_returns_503_when_unavailable() {
    browser_route_returns_503("GET", "/v1/browser/pdf", None).await;
}

#[tokio::test]
async fn browser_evaluate_returns_503_when_unavailable() {
    browser_route_returns_503(
        "POST",
        "/v1/browser/evaluate",
        Some(serde_json::json!({ "expression": "1 + 1" })),
    )
    .await;
}

#[tokio::test]
async fn browser_find_returns_503_when_unavailable() {
    browser_route_returns_503(
        "POST",
        "/v1/browser/find",
        Some(serde_json::json!({ "query": "submit button" })),
    )
    .await;
}
