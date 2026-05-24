use std::sync::Arc;

use axum::extract::State;
use axum::Json;
use serde_json::Value;

use ix_core::types::{EgressPatch, EgressPolicy};
use ix_core::Result;

use crate::state::AppState;

pub async fn get_policy(State(state): State<Arc<AppState>>) -> Json<Value> {
    let policy = match &state.egress {
        Some(filter) => filter.get_policy(),
        None => EgressPolicy::default(),
    };
    Json(serde_json::to_value(policy).unwrap())
}

pub async fn patch_policy(
    State(state): State<Arc<AppState>>,
    Json(patch): Json<EgressPatch>,
) -> Result<Json<Value>> {
    match &state.egress {
        Some(filter) => {
            filter.update_policy(patch);
            Ok(Json(serde_json::json!({ "updated": true })))
        }
        None => Err(ix_core::Error::Unavailable(
            "egress filter is not enabled".into(),
        )),
    }
}
