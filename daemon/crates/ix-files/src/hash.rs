use ix_core::{
    types::{FileHash, HashRequest, HashResult},
    Result,
};
use ring::digest::{Context, SHA256};
use tokio::io::AsyncReadExt;

/// Read window fed to the digest. Fixed, and reused across every path in the
/// batch, so hashing a 400 MB dataset costs the same memory as hashing a README
/// — the sandbox has 512 MB total and the workspace is allowed to be larger
/// than that.
const CHUNK: usize = 64 * 1024;

/// sha256 each path, skipping the ones that will not read.
///
/// Best-effort by contract. The caller enumerated these paths a moment ago
/// while the tool call it is bracketing was still writing, so a path that has
/// since been deleted, replaced by a directory, or had its permissions changed
/// is the ordinary case rather than a fault. Reporting that as an error would
/// throw away every other digest in the batch, so a path that cannot be read is
/// simply absent from `hashes` and the caller reads absence as "unknown".
pub async fn hash_files(req: HashRequest) -> Result<HashResult> {
    let mut buf = vec![0u8; CHUNK];
    let mut hashes = Vec::with_capacity(req.paths.len());

    for path in &req.paths {
        if let Some(hash) = hash_one(path, &mut buf).await {
            hashes.push(hash);
        }
    }

    Ok(HashResult { hashes })
}

/// Digest one file, streaming it through `buf`. `None` for anything unreadable.
async fn hash_one(path: &str, buf: &mut [u8]) -> Option<FileHash> {
    // Opening a directory succeeds on Linux; it is the first read that fails
    // with EISDIR, which lands in the same `None` as a missing file.
    let mut file = tokio::fs::File::open(path).await.ok()?;
    let mut ctx = Context::new(&SHA256);
    let mut size: u64 = 0;

    loop {
        let n = file.read(buf).await.ok()?;
        if n == 0 {
            break;
        }
        ctx.update(&buf[..n]);
        size += n as u64;
    }

    Some(FileHash {
        path: path.to_string(),
        hash: hex(ctx.finish().as_ref()),
        // Counted from the bytes actually digested rather than from metadata,
        // so `size` and `hash` always describe the same read even if the file
        // is rewritten the instant after.
        size,
    })
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write;

    let mut out = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        let _ = write!(out, "{:02x}", b);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    fn hash_req(paths: &[&str]) -> HashRequest {
        HashRequest {
            paths: paths.iter().map(|p| p.to_string()).collect(),
        }
    }

    #[tokio::test]
    async fn hashes_known_content() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("hello.txt");
        std::fs::write(&path, b"hello world").unwrap();

        let result = hash_files(hash_req(&[path.to_str().unwrap()]))
            .await
            .unwrap();

        assert_eq!(result.hashes.len(), 1);
        assert_eq!(
            result.hashes[0].hash,
            "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
        );
        assert_eq!(result.hashes[0].size, 11);
        assert_eq!(result.hashes[0].path, path.to_str().unwrap());
    }

    #[tokio::test]
    async fn hashes_file_larger_than_one_chunk() {
        // 200_000 bytes of `i % 251` — several reads through the buffer, so a
        // digest that only covered the first chunk would not match.
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("big.bin");
        let content: Vec<u8> = (0..200_000usize).map(|i| (i % 251) as u8).collect();
        assert!(content.len() > CHUNK);
        std::fs::write(&path, &content).unwrap();

        let result = hash_files(hash_req(&[path.to_str().unwrap()]))
            .await
            .unwrap();

        assert_eq!(result.hashes.len(), 1);
        assert_eq!(
            result.hashes[0].hash,
            "e24bc62381f1224fbbb74688663f8f9743b9680b193edd666835e97b06e730eb"
        );
        assert_eq!(result.hashes[0].size, 200_000);
    }

    #[tokio::test]
    async fn hashes_empty_file() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("empty.txt");
        std::fs::write(&path, b"").unwrap();

        let result = hash_files(hash_req(&[path.to_str().unwrap()]))
            .await
            .unwrap();

        assert_eq!(result.hashes.len(), 1);
        assert_eq!(
            result.hashes[0].hash,
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
        assert_eq!(result.hashes[0].size, 0);
    }

    #[tokio::test]
    async fn missing_path_is_omitted_and_the_rest_still_hash() {
        let dir = TempDir::new().unwrap();
        let good = dir.path().join("good.txt");
        std::fs::write(&good, b"hello world").unwrap();
        let gone = dir.path().join("gone.txt");

        let result = hash_files(hash_req(&[
            gone.to_str().unwrap(),
            good.to_str().unwrap(),
        ]))
        .await
        .unwrap();

        assert_eq!(result.hashes.len(), 1, "missing path should be omitted");
        assert_eq!(result.hashes[0].path, good.to_str().unwrap());
    }

    #[tokio::test]
    async fn directory_is_omitted() {
        let dir = TempDir::new().unwrap();
        let sub = dir.path().join("subdir");
        std::fs::create_dir(&sub).unwrap();

        let result = hash_files(hash_req(&[sub.to_str().unwrap()]))
            .await
            .unwrap();

        assert!(result.hashes.is_empty());
    }

    #[tokio::test]
    async fn empty_request_returns_empty_list() {
        let result = hash_files(hash_req(&[])).await.unwrap();
        assert!(result.hashes.is_empty());
    }

    #[tokio::test]
    async fn hashes_follow_request_order() {
        let dir = TempDir::new().unwrap();
        let a = dir.path().join("a.txt");
        let b = dir.path().join("b.txt");
        std::fs::write(&a, b"a").unwrap();
        std::fs::write(&b, b"b").unwrap();

        let result = hash_files(hash_req(&[b.to_str().unwrap(), a.to_str().unwrap()]))
            .await
            .unwrap();

        let paths: Vec<&str> = result.hashes.iter().map(|h| h.path.as_str()).collect();
        assert_eq!(paths, vec![b.to_str().unwrap(), a.to_str().unwrap()]);
    }
}
