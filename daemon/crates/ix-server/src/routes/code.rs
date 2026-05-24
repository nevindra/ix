use std::sync::Arc;

use axum::extract::{Path, State};
use axum::Json;

use ix_core::sse::SseResponse;
use ix_core::types::CodeRequest;

use crate::state::AppState;

pub async fn code_exec(
    State(state): State<Arc<AppState>>,
    Json(req): Json<CodeRequest>,
) -> SseResponse {
    let (sender, sse) = ix_core::sse::sse_channel(32);
    let kernels = Arc::clone(&state.kernels);
    tokio::spawn(async move {
        kernels.execute(req, sender).await;
    });
    sse
}

pub async fn e2b_code(
    State(state): State<Arc<AppState>>,
    Path(_id): Path<String>,
    body: Json<CodeRequest>,
) -> SseResponse {
    code_exec(State(state), body).await
}
