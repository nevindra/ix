use std::sync::Arc;

use axum::extract::State;
use axum::Json;
use serde_json::Value;

use ix_core::types::{HttpFetchRequest, WebSearchRequest};
use ix_core::Result;

use crate::state::AppState;

pub async fn http_fetch(
    State(_state): State<Arc<AppState>>,
    Json(req): Json<HttpFetchRequest>,
) -> Result<Json<Value>> {
    let result = ix_fetch::http_fetch(req).await?;
    Ok(Json(serde_json::to_value(result).unwrap()))
}

pub async fn web_search(
    State(_state): State<Arc<AppState>>,
    Json(req): Json<WebSearchRequest>,
) -> Result<Json<Value>> {
    let result = ix_fetch::search::web_search(req).await?;
    Ok(Json(serde_json::to_value(result).unwrap()))
}
