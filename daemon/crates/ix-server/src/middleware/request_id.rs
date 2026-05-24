use axum::body::Body;
use axum::http::{HeaderName, HeaderValue, Request};
use axum::middleware::Next;
use axum::response::Response;

static X_REQUEST_ID: HeaderName = HeaderName::from_static("x-request-id");

pub async fn inject_request_id(request: Request<Body>, next: Next) -> Response {
    let mut response = next.run(request).await;

    if !response.headers().contains_key(&X_REQUEST_ID) {
        let id = uuid::Uuid::new_v4().to_string();
        if let Ok(val) = HeaderValue::from_str(&id) {
            response.headers_mut().insert(X_REQUEST_ID.clone(), val);
        }
    }

    response
}
