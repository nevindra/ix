use bytes::Bytes;
use ix_core::Result;
use std::path::Path;

/// Write uploaded bytes to `path`, creating parent directories as needed.
/// Returns the number of bytes written.
pub async fn handle_upload(path: String, data: Bytes) -> Result<u64> {
    let p = Path::new(&path);
    if let Some(parent) = p.parent() {
        if !parent.as_os_str().is_empty() {
            tokio::fs::create_dir_all(parent).await?;
        }
    }
    tokio::fs::write(&path, &data).await?;
    Ok(data.len() as u64)
}

/// Open `path` for streaming download.
/// Returns `(filename, file)` where `filename` is the basename.
pub async fn handle_download(path: &str) -> Result<(String, tokio::fs::File)> {
    let p = Path::new(path);
    let filename = p
        .file_name()
        .map(|n| n.to_string_lossy().to_string())
        .unwrap_or_else(|| "download".to_string());

    let file = tokio::fs::File::open(p).await?;
    Ok((filename, file))
}

#[cfg(test)]
mod tests {
    use super::*;
    use bytes::Bytes;
    use tempfile::TempDir;
    use tokio::io::AsyncReadExt;

    #[tokio::test]
    async fn upload_writes_bytes_correctly() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("upload.bin");
        let data = Bytes::from_static(b"hello upload");
        let written = handle_upload(path.to_str().unwrap().to_string(), data.clone())
            .await
            .unwrap();
        assert_eq!(written, data.len() as u64);
        let on_disk = tokio::fs::read(path).await.unwrap();
        assert_eq!(on_disk, data.as_ref());
    }

    #[tokio::test]
    async fn upload_creates_parent_directories() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("nested/dirs/file.bin");
        let data = Bytes::from_static(b"deep data");
        handle_upload(path.to_str().unwrap().to_string(), data.clone())
            .await
            .unwrap();
        assert!(path.exists());
    }

    #[tokio::test]
    async fn download_returns_correct_file_handle() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("download_me.txt");
        tokio::fs::write(&path, b"download content").await.unwrap();

        let (filename, mut file) =
            handle_download(path.to_str().unwrap()).await.unwrap();

        assert_eq!(filename, "download_me.txt");

        let mut buf = Vec::new();
        file.read_to_end(&mut buf).await.unwrap();
        assert_eq!(buf, b"download content");
    }

    #[tokio::test]
    async fn download_nonexistent_returns_error() {
        let result = handle_download("/tmp/ix_files_nonexistent_download.bin").await;
        assert!(result.is_err());
    }
}
