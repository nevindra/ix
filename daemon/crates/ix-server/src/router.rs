use std::sync::Arc;

use axum::middleware;
use axum::routing::{get, post};
use axum::Router;

use crate::middleware::request_id::inject_request_id;
use crate::routes;
use crate::state::AppState;

pub fn build_router(state: Arc<AppState>) -> Router {
    Router::new()
        // Health
        .route("/health", get(routes::health::health))
        // E2B-compatible routes (sandbox ID captured but ignored)
        .route(
            "/sandboxes/{id}/commands/run",
            post(routes::shell::e2b_shell),
        )
        .route(
            "/sandboxes/{id}/code/execute",
            post(routes::code::e2b_code),
        )
        .route(
            "/sandboxes/{id}/files",
            get(routes::files::e2b_read_file).post(routes::files::e2b_write_file),
        )
        .route(
            "/sandboxes/{id}/files/upload",
            post(routes::files::e2b_upload),
        )
        .route(
            "/sandboxes/{id}/files/download",
            get(routes::files::e2b_download),
        )
        .route(
            "/sandboxes/{id}/files/list",
            get(routes::files::e2b_list),
        )
        // ix-native routes
        .route("/v1/shell/exec", post(routes::shell::shell_exec))
        .route("/v1/code/execute", post(routes::code::code_exec))
        .route("/v1/file/read", post(routes::files::read_file))
        .route("/v1/file/write", post(routes::files::write_file))
        .route("/v1/file/edit", post(routes::files::edit_file))
        .route("/v1/file/glob", post(routes::files::glob_files))
        .route("/v1/file/grep", post(routes::files::grep_files))
        .route("/v1/file/tree", post(routes::files::tree))
        .route("/v1/file/stat", get(routes::files::stat_file))
        .route("/v1/file/upload", post(routes::files::upload_file))
        .route("/v1/file/download", get(routes::files::download_file))
        .route("/v1/file/ls", post(routes::files::list_dir))
        .route("/v1/browser/navigate", post(routes::browser::navigate))
        .route("/v1/browser/screenshot", get(routes::browser::screenshot))
        .route("/v1/browser/action", post(routes::browser::action))
        .route("/v1/browser/snapshot", get(routes::browser::snapshot))
        .route("/v1/browser/text", get(routes::browser::text))
        .route("/v1/browser/pdf", get(routes::browser::pdf))
        .route("/v1/browser/evaluate", post(routes::browser::evaluate))
        .route("/v1/browser/find", post(routes::browser::find))
        .route("/v1/browser/wait", post(routes::browser::wait))
        .route("/v1/http/fetch", post(routes::fetch::http_fetch))
        .route("/v1/web/search", post(routes::fetch::web_search))
        .route(
            "/v1/workspace/info",
            get(routes::workspace::workspace_info),
        )
        .route(
            "/v1/egress/policy",
            get(routes::egress::get_policy).patch(routes::egress::patch_policy),
        )
        .layer(middleware::from_fn(inject_request_id))
        .with_state(state)
}
