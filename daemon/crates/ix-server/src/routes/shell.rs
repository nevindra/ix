use std::sync::Arc;

use axum::extract::{Path, State};
use axum::Json;

use ix_core::sse::SseResponse;
use ix_core::types::ShellRequest;

use crate::state::AppState;

pub async fn shell_exec(
    State(_state): State<Arc<AppState>>,
    Json(req): Json<ShellRequest>,
) -> SseResponse {
    let (sender, sse) = ix_core::sse::sse_channel(32);
    tokio::spawn(async move {
        ix_shell::execute_shell(req, sender).await;
    });
    sse
}

pub async fn e2b_shell(
    State(state): State<Arc<AppState>>,
    Path(_id): Path<String>,
    body: Json<ShellRequest>,
) -> SseResponse {
    shell_exec(State(state), body).await
}
