use std::collections::HashMap;
use std::sync::Arc;

use axum::body::Body;
use axum::extract::{Path, Query, State};
use axum::http::header;
use axum::response::IntoResponse;
use axum::Json;
use axum_extra::extract::Multipart;
use serde::Deserialize;
use serde_json::{json, Value};
use tokio_util::io::ReaderStream;

use ix_core::types::{
    EditFileRequest, GlobRequest, GrepRequest, ReadFileRequest, TreeRequest, WriteFileRequest,
};
use ix_core::Result;

use crate::state::AppState;

// ─── ix-native handlers ────────────────────────────────────────────────────────

pub async fn read_file(
    State(_state): State<Arc<AppState>>,
    Json(req): Json<ReadFileRequest>,
) -> Result<Json<ix_core::types::FileContent>> {
    let content = ix_files::read_file(req).await?;
    Ok(Json(content))
}

pub async fn write_file(
    State(_state): State<Arc<AppState>>,
    Json(req): Json<WriteFileRequest>,
) -> Result<Json<Value>> {
    let bytes_written = ix_files::write_file(req).await?;
    Ok(Json(json!({ "bytes_written": bytes_written })))
}

pub async fn edit_file(
    State(_state): State<Arc<AppState>>,
    Json(req): Json<EditFileRequest>,
) -> Result<Json<Value>> {
    let path = req.path.clone();
    ix_files::edit_file(req).await?;
    Ok(Json(json!({ "applied": true, "path": path })))
}

pub async fn glob_files(
    State(_state): State<Arc<AppState>>,
    Json(req): Json<GlobRequest>,
) -> Result<Json<ix_core::types::GlobResult>> {
    let result = ix_files::glob_files(req).await?;
    Ok(Json(result))
}

pub async fn grep_files(
    State(_state): State<Arc<AppState>>,
    Json(req): Json<GrepRequest>,
) -> Result<Json<ix_core::types::GrepResult>> {
    let result = ix_files::grep_files(req).await?;
    Ok(Json(result))
}

pub async fn tree(
    State(_state): State<Arc<AppState>>,
    Json(req): Json<TreeRequest>,
) -> Result<Json<ix_core::types::TreeResult>> {
    let result = ix_files::tree(req).await?;
    Ok(Json(result))
}

pub async fn stat_file(
    State(_state): State<Arc<AppState>>,
    Query(params): Query<HashMap<String, String>>,
) -> Result<Json<ix_files::FileStat>> {
    let path = params
        .get("path")
        .ok_or_else(|| ix_core::Error::BadRequest("missing 'path' query parameter".into()))?;
    let stat = ix_files::stat_file(path).await?;
    Ok(Json(stat))
}

pub async fn upload_file(
    State(_state): State<Arc<AppState>>,
    mut multipart: Multipart,
) -> Result<Json<Value>> {
    let mut path: Option<String> = None;
    let mut data: Option<bytes::Bytes> = None;

    while let Some(field) = multipart
        .next_field()
        .await
        .map_err(|e| ix_core::Error::BadRequest(format!("multipart error: {e}")))?
    {
        let name = field.name().unwrap_or("").to_string();
        match name.as_str() {
            "path" => {
                path = Some(
                    field
                        .text()
                        .await
                        .map_err(|e| ix_core::Error::BadRequest(format!("bad path field: {e}")))?,
                );
            }
            "file" => {
                data = Some(
                    field
                        .bytes()
                        .await
                        .map_err(|e| ix_core::Error::BadRequest(format!("bad file field: {e}")))?,
                );
            }
            _ => {}
        }
    }

    let path =
        path.ok_or_else(|| ix_core::Error::BadRequest("missing 'path' field in form".into()))?;
    let data =
        data.ok_or_else(|| ix_core::Error::BadRequest("missing 'file' field in form".into()))?;

    let bytes_written = ix_files::handle_upload(path, data).await?;
    Ok(Json(json!({ "bytes_written": bytes_written })))
}

pub async fn download_file(
    State(_state): State<Arc<AppState>>,
    Query(params): Query<HashMap<String, String>>,
) -> Result<impl IntoResponse> {
    let path = params
        .get("path")
        .ok_or_else(|| ix_core::Error::BadRequest("missing 'path' query parameter".into()))?;

    let (filename, file) = ix_files::handle_download(path).await?;
    let stream = ReaderStream::new(file);
    let body = Body::from_stream(stream);

    let disposition = format!("attachment; filename=\"{}\"", filename);
    Ok((
        [
            (header::CONTENT_DISPOSITION, disposition),
            (
                header::CONTENT_TYPE,
                "application/octet-stream".to_string(),
            ),
        ],
        body,
    ))
}

#[derive(Deserialize)]
pub struct ListDirRequest {
    pub path: String,
}

pub async fn list_dir(
    State(_state): State<Arc<AppState>>,
    Json(req): Json<ListDirRequest>,
) -> Result<Json<Value>> {
    let entries = ix_files::list_dir(&req.path).await?;
    Ok(Json(json!({ "entries": entries })))
}

// ─── E2B-compatible handlers ───────────────────────────────────────────────────

pub async fn e2b_read_file(
    State(state): State<Arc<AppState>>,
    Path(_id): Path<String>,
    Query(params): Query<HashMap<String, String>>,
) -> Result<Json<ix_core::types::FileContent>> {
    let path = params
        .get("path")
        .ok_or_else(|| ix_core::Error::BadRequest("missing 'path' query parameter".into()))?;
    let req = ReadFileRequest {
        path: path.clone(),
        offset: params.get("offset").and_then(|v| v.parse().ok()),
        limit: params.get("limit").and_then(|v| v.parse().ok()),
    };
    read_file(State(state), Json(req)).await
}

pub async fn e2b_write_file(
    State(state): State<Arc<AppState>>,
    Path(_id): Path<String>,
    body: Json<WriteFileRequest>,
) -> Result<Json<Value>> {
    write_file(State(state), body).await
}

pub async fn e2b_upload(
    State(state): State<Arc<AppState>>,
    Path(_id): Path<String>,
    multipart: Multipart,
) -> Result<Json<Value>> {
    upload_file(State(state), multipart).await
}

pub async fn e2b_download(
    State(state): State<Arc<AppState>>,
    Path(_id): Path<String>,
    query: Query<HashMap<String, String>>,
) -> Result<impl IntoResponse> {
    download_file(State(state), query).await
}

pub async fn e2b_list(
    State(state): State<Arc<AppState>>,
    Path(_id): Path<String>,
    Query(params): Query<HashMap<String, String>>,
) -> Result<Json<Value>> {
    let path = params
        .get("path")
        .cloned()
        .unwrap_or_else(|| ".".to_string());
    list_dir(State(state), Json(ListDirRequest { path })).await
}
