use std::collections::HashMap;
use std::sync::Arc;

use axum::extract::{Query, State};
use axum::http::header;
use axum::response::IntoResponse;
use axum::Json;
use serde::Deserialize;
use serde_json::{json, Value};

use ix_core::types::{BrowserAction, SnapshotOpts, TextOpts};
use ix_core::Result;

use crate::state::AppState;

fn check_available(state: &AppState) -> Result<()> {
    if !state.browser.available() {
        return Err(ix_core::Error::Unavailable(
            "browser backend is not available".into(),
        ));
    }
    Ok(())
}

#[derive(Deserialize)]
pub struct NavigateRequest {
    pub url: String,
}

pub async fn navigate(
    State(state): State<Arc<AppState>>,
    Json(req): Json<NavigateRequest>,
) -> Result<Json<Value>> {
    check_available(&state)?;
    let result = state.browser.navigate(&req.url).await?;
    Ok(Json(serde_json::to_value(result).unwrap()))
}

pub async fn screenshot(State(state): State<Arc<AppState>>) -> Result<impl IntoResponse> {
    check_available(&state)?;
    let png_bytes = state.browser.screenshot().await?;
    Ok(([(header::CONTENT_TYPE, "image/png")], png_bytes))
}

pub async fn action(
    State(state): State<Arc<AppState>>,
    Json(req): Json<BrowserAction>,
) -> Result<Json<Value>> {
    check_available(&state)?;
    let result = state.browser.action(req).await?;
    Ok(Json(serde_json::to_value(result).unwrap()))
}

pub async fn snapshot(
    State(state): State<Arc<AppState>>,
    Query(params): Query<HashMap<String, String>>,
) -> Result<Json<Value>> {
    check_available(&state)?;
    let opts = SnapshotOpts {
        filter: params.get("filter").cloned(),
        selector: params.get("selector").cloned(),
        depth: params.get("depth").and_then(|v| v.parse().ok()),
    };
    let result = state.browser.snapshot(opts).await?;
    Ok(Json(serde_json::to_value(result).unwrap()))
}

pub async fn text(
    State(state): State<Arc<AppState>>,
    Query(params): Query<HashMap<String, String>>,
) -> Result<Json<Value>> {
    check_available(&state)?;
    let opts = TextOpts {
        raw: params.get("raw").map(|v| v == "true"),
        max_chars: params.get("max_chars").and_then(|v| v.parse().ok()),
    };
    let result = state.browser.text(opts).await?;
    Ok(Json(serde_json::to_value(result).unwrap()))
}

pub async fn pdf(State(state): State<Arc<AppState>>) -> Result<impl IntoResponse> {
    check_available(&state)?;
    let pdf_bytes = state.browser.pdf().await?;
    Ok(([(header::CONTENT_TYPE, "application/pdf")], pdf_bytes))
}

#[derive(Deserialize)]
pub struct EvalRequest {
    pub expression: String,
}

pub async fn evaluate(
    State(state): State<Arc<AppState>>,
    Json(req): Json<EvalRequest>,
) -> Result<Json<Value>> {
    check_available(&state)?;
    let result = state.browser.eval(&req.expression).await?;
    Ok(Json(json!({ "result": result })))
}

#[derive(Deserialize)]
pub struct FindRequest {
    pub query: String,
}

pub async fn find(
    State(state): State<Arc<AppState>>,
    Json(req): Json<FindRequest>,
) -> Result<Json<Value>> {
    check_available(&state)?;
    let result = state.browser.find(&req.query).await?;
    Ok(Json(serde_json::to_value(result).unwrap()))
}
