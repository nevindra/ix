use std::sync::Arc;

use axum::extract::State;
use axum::Json;
use serde_json::{json, Value};

use crate::state::AppState;

pub async fn health(State(state): State<Arc<AppState>>) -> Json<Value> {
    let uptime = state.start_time.elapsed().as_secs();
    Json(json!({
        "status": "ok",
        "uptime_sec": uptime,
        "browser": state.browser.available(),
    }))
}
