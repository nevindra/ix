use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("not found: {0}")]
    NotFound(String),

    #[error("bad request: {0}")]
    BadRequest(String),

    #[error("internal error: {0}")]
    Internal(String),

    #[error("service unavailable: {0}")]
    Unavailable(String),

    #[error("timeout: {0}")]
    Timeout(String),

    #[error(transparent)]
    Io(#[from] std::io::Error),

    #[error(transparent)]
    Json(#[from] serde_json::Error),
}

impl IntoResponse for Error {
    fn into_response(self) -> Response {
        let status = match &self {
            Error::NotFound(_) => StatusCode::NOT_FOUND,
            Error::BadRequest(_) => StatusCode::BAD_REQUEST,
            Error::Internal(_) => StatusCode::INTERNAL_SERVER_ERROR,
            Error::Unavailable(_) => StatusCode::SERVICE_UNAVAILABLE,
            Error::Timeout(_) => StatusCode::GATEWAY_TIMEOUT,
            Error::Io(_) => StatusCode::INTERNAL_SERVER_ERROR,
            Error::Json(_) => StatusCode::BAD_REQUEST,
        };
        let body = serde_json::json!({ "error": self.to_string() });
        (status, axum::Json(body)).into_response()
    }
}

pub type Result<T> = std::result::Result<T, Error>;

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::StatusCode;
    use axum::response::IntoResponse;

    fn status_of(err: Error) -> StatusCode {
        let resp = err.into_response();
        resp.status()
    }

    #[test]
    fn not_found_maps_to_404() {
        assert_eq!(status_of(Error::NotFound("x".into())), StatusCode::NOT_FOUND);
    }

    #[test]
    fn bad_request_maps_to_400() {
        assert_eq!(
            status_of(Error::BadRequest("x".into())),
            StatusCode::BAD_REQUEST
        );
    }

    #[test]
    fn internal_maps_to_500() {
        assert_eq!(
            status_of(Error::Internal("x".into())),
            StatusCode::INTERNAL_SERVER_ERROR
        );
    }

    #[test]
    fn unavailable_maps_to_503() {
        assert_eq!(
            status_of(Error::Unavailable("x".into())),
            StatusCode::SERVICE_UNAVAILABLE
        );
    }

    #[test]
    fn timeout_maps_to_504() {
        assert_eq!(
            status_of(Error::Timeout("x".into())),
            StatusCode::GATEWAY_TIMEOUT
        );
    }

    #[test]
    fn io_error_maps_to_500() {
        let io_err = std::io::Error::new(std::io::ErrorKind::Other, "disk fail");
        let err: Error = io_err.into();
        assert_eq!(status_of(err), StatusCode::INTERNAL_SERVER_ERROR);
    }

    #[test]
    fn json_error_maps_to_400() {
        let json_err: serde_json::Error =
            serde_json::from_str::<serde_json::Value>("not json").unwrap_err();
        let err: Error = json_err.into();
        assert_eq!(status_of(err), StatusCode::BAD_REQUEST);
    }

    #[test]
    fn error_message_preserved_in_not_found() {
        let msg = "the thing is gone";
        let err = Error::NotFound(msg.into());
        assert!(err.to_string().contains(msg));
    }

    #[test]
    fn error_message_preserved_in_bad_request() {
        let msg = "missing field foo";
        let err = Error::BadRequest(msg.into());
        assert!(err.to_string().contains(msg));
    }

    #[test]
    fn error_message_preserved_in_internal() {
        let msg = "something blew up";
        let err = Error::Internal(msg.into());
        assert!(err.to_string().contains(msg));
    }

    #[test]
    fn from_io_error_conversion() {
        let io_err = std::io::Error::new(std::io::ErrorKind::NotFound, "file missing");
        let err: Error = io_err.into();
        assert!(matches!(err, Error::Io(_)));
    }

    #[test]
    fn from_serde_json_error_conversion() {
        let json_err: serde_json::Error =
            serde_json::from_str::<serde_json::Value>("{bad}").unwrap_err();
        let err: Error = json_err.into();
        assert!(matches!(err, Error::Json(_)));
    }
}
