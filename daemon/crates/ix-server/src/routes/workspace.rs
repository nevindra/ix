use std::collections::HashMap;
use std::sync::Arc;

use axum::extract::State;
use axum::Json;

use ix_core::types::WorkspaceInfo;

use crate::state::AppState;

pub async fn workspace_info(State(state): State<Arc<AppState>>) -> Json<WorkspaceInfo> {
    let tool_names = ["rg", "fd", "git", "python3", "node", "tree", "curl", "wget"];
    let mut tools = HashMap::new();

    for name in &tool_names {
        let available = tokio::process::Command::new("which")
            .arg(name)
            .output()
            .await
            .map(|o| o.status.success())
            .unwrap_or(false);
        tools.insert(name.to_string(), available);
    }

    let info = WorkspaceInfo {
        os: std::env::consts::OS.to_string(),
        arch: std::env::consts::ARCH.to_string(),
        working_dir: state.config.workspace.clone(),
        tools,
        browser: state.browser.available(),
    };

    Json(info)
}
