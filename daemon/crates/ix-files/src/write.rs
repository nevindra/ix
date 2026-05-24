use ix_core::{
    types::{EditFileRequest, WriteFileRequest},
    Error, Result,
};
use std::path::Path;

/// Write content to a file, creating parent directories as needed.
/// Returns the number of bytes written.
pub async fn write_file(req: WriteFileRequest) -> Result<usize> {
    let path = Path::new(&req.path);
    if let Some(parent) = path.parent() {
        if !parent.as_os_str().is_empty() {
            tokio::fs::create_dir_all(parent).await?;
        }
    }
    let bytes = req.content.as_bytes();
    tokio::fs::write(path, bytes).await?;
    Ok(bytes.len())
}

/// Edit a file by replacing a unique occurrence of `old` with `new`.
/// Errors if `old` is not found or appears more than once.
pub async fn edit_file(req: EditFileRequest) -> Result<()> {
    let content = tokio::fs::read_to_string(&req.path).await?;

    let count = content.matches(req.old.as_str()).count();
    if count == 0 {
        return Err(Error::BadRequest(format!(
            "old string not found in {}",
            req.path
        )));
    }
    if count > 1 {
        return Err(Error::BadRequest(format!(
            "old string is ambiguous ({} occurrences) in {}",
            count, req.path
        )));
    }

    let new_content = content.replacen(req.old.as_str(), req.new.as_str(), 1);
    tokio::fs::write(&req.path, new_content.as_bytes()).await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use ix_core::types::{EditFileRequest, WriteFileRequest};
    use tempfile::TempDir;

    fn write_req(path: &str, content: &str) -> WriteFileRequest {
        WriteFileRequest {
            path: path.to_string(),
            content: content.to_string(),
        }
    }

    fn edit_req(path: &str, old: &str, new: &str) -> EditFileRequest {
        EditFileRequest {
            path: path.to_string(),
            old: old.to_string(),
            new: new.to_string(),
        }
    }

    #[tokio::test]
    async fn write_creates_file_with_correct_content() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("test.txt");
        let path_str = path.to_str().unwrap();
        let bytes = write_file(write_req(path_str, "hello world")).await.unwrap();
        assert_eq!(bytes, 11);
        let content = tokio::fs::read_to_string(path_str).await.unwrap();
        assert_eq!(content, "hello world");
    }

    #[tokio::test]
    async fn write_creates_parent_directories() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("a/b/c/file.txt");
        let path_str = path.to_str().unwrap();
        write_file(write_req(path_str, "deep")).await.unwrap();
        assert!(path.exists());
        let content = tokio::fs::read_to_string(path_str).await.unwrap();
        assert_eq!(content, "deep");
    }

    #[tokio::test]
    async fn write_overwrites_existing_file() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("overwrite.txt");
        let path_str = path.to_str().unwrap();
        write_file(write_req(path_str, "original")).await.unwrap();
        write_file(write_req(path_str, "replaced")).await.unwrap();
        let content = tokio::fs::read_to_string(path_str).await.unwrap();
        assert_eq!(content, "replaced");
    }

    #[tokio::test]
    async fn edit_replaces_unique_match() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("edit.txt");
        let path_str = path.to_str().unwrap();
        write_file(write_req(path_str, "foo bar baz")).await.unwrap();
        edit_file(edit_req(path_str, "bar", "QUX")).await.unwrap();
        let content = tokio::fs::read_to_string(path_str).await.unwrap();
        assert_eq!(content, "foo QUX baz");
    }

    #[tokio::test]
    async fn edit_errors_on_non_unique_match() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("dup.txt");
        let path_str = path.to_str().unwrap();
        write_file(write_req(path_str, "dup dup dup")).await.unwrap();
        let result = edit_file(edit_req(path_str, "dup", "X")).await;
        assert!(result.is_err());
        let msg = result.unwrap_err().to_string();
        assert!(msg.contains("ambiguous"), "expected 'ambiguous' in: {msg}");
    }

    #[tokio::test]
    async fn edit_errors_on_missing_match() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("missing.txt");
        let path_str = path.to_str().unwrap();
        write_file(write_req(path_str, "hello world")).await.unwrap();
        let result = edit_file(edit_req(path_str, "NOTHERE", "X")).await;
        assert!(result.is_err());
        let msg = result.unwrap_err().to_string();
        assert!(msg.contains("not found"), "expected 'not found' in: {msg}");
    }

    #[tokio::test]
    async fn edit_preserves_rest_of_file() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("preserve.txt");
        let path_str = path.to_str().unwrap();
        let original = "line1\nTARGET\nline3\n";
        write_file(write_req(path_str, original)).await.unwrap();
        edit_file(edit_req(path_str, "TARGET", "REPLACED")).await.unwrap();
        let content = tokio::fs::read_to_string(path_str).await.unwrap();
        assert!(content.contains("line1"));
        assert!(content.contains("REPLACED"));
        assert!(content.contains("line3"));
        assert!(!content.contains("TARGET"));
    }
}
