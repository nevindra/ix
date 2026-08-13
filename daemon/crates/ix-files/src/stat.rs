use ix_core::Result;
use serde::Serialize;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;

#[derive(Debug, Serialize)]
pub struct FileStat {
    pub path: String,
    pub size: u64,
    /// Unix permission bits as an octal string, e.g. "0644"
    pub mode: String,
    /// Last-modified time in RFC 3339 format
    pub mod_time: String,
    pub is_dir: bool,
}

#[derive(Debug, Serialize)]
pub struct DirEntry {
    pub name: String,
    /// "file" or "dir"
    pub entry_type: String,
    pub size: u64,
}

/// Return metadata for a single path (file or directory).
pub async fn stat_file(path: &str) -> Result<FileStat> {
    let meta = tokio::fs::metadata(path).await?;
    let mode = format!("{:04o}", meta.permissions().mode() & 0o7777);
    let mod_time = format_system_time(meta.modified()?);

    Ok(FileStat {
        path: path.to_string(),
        size: meta.len(),
        mode,
        mod_time,
        is_dir: meta.is_dir(),
    })
}

/// List entries in a directory (non-recursive).
pub async fn list_dir(path: &str) -> Result<Vec<DirEntry>> {
    let mut rd = tokio::fs::read_dir(Path::new(path)).await?;
    let mut entries = Vec::new();

    while let Some(entry) = rd.next_entry().await? {
        let meta = entry.metadata().await?;
        let name = entry.file_name().to_string_lossy().to_string();
        let entry_type = if meta.is_dir() { "dir" } else { "file" }.to_string();
        entries.push(DirEntry {
            name,
            entry_type,
            size: meta.len(),
        });
    }

    // Sort by name for deterministic output.
    entries.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(entries)
}

pub(crate) fn format_system_time(t: std::time::SystemTime) -> String {
    use std::time::UNIX_EPOCH;

    let secs = t
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0) as i64;

    // Manual RFC 3339 formatting without chrono dependency.
    // Decompose Unix timestamp into date/time components.
    let (year, month, day, hour, min, sec) = unix_to_datetime(secs);
    format!(
        "{:04}-{:02}-{:02}T{:02}:{:02}:{:02}Z",
        year, month, day, hour, min, sec
    )
}

/// Nanoseconds since the Unix epoch, the same clock `format_system_time` reads
/// but without its truncation to whole seconds. Pre-epoch times report 0, as
/// they do there.
pub(crate) fn unix_nanos(t: std::time::SystemTime) -> u64 {
    t.duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as u64)
        .unwrap_or(0)
}

/// Minimal Unix-timestamp → (year, month, day, hour, min, sec) converter (UTC only).
fn unix_to_datetime(secs: i64) -> (i32, u32, u32, u32, u32, u32) {
    let s = secs;
    let sec = (s % 60) as u32;
    let s = s / 60;
    let min = (s % 60) as u32;
    let s = s / 60;
    let hour = (s % 24) as u32;
    let mut days = (s / 24) as i32; // days since 1970-01-01

    // Algorithm from http://howardhinnant.github.io/date_algorithms.html
    days += 719468;
    let era = if days >= 0 { days } else { days - 146096 } / 146097;
    let doe = (days - era * 146097) as u32;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe as i32 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };

    (y, m, d, hour, min, sec)
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    #[tokio::test]
    async fn stat_returns_correct_size() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("sized.txt");
        let content = b"12345"; // 5 bytes
        std::fs::write(&path, content).unwrap();

        let stat = stat_file(path.to_str().unwrap()).await.unwrap();
        assert_eq!(stat.size, 5);
        assert!(!stat.is_dir);
    }

    #[tokio::test]
    async fn stat_returns_is_dir_true_for_directory() {
        let dir = TempDir::new().unwrap();
        let stat = stat_file(dir.path().to_str().unwrap()).await.unwrap();
        assert!(stat.is_dir);
    }

    #[tokio::test]
    async fn stat_returns_mode_as_octal_string() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("mode.txt");
        std::fs::write(&path, b"data").unwrap();

        let stat = stat_file(path.to_str().unwrap()).await.unwrap();
        // Mode should be a 4-character octal string like "0644"
        assert_eq!(stat.mode.len(), 4, "mode should be 4 chars: {}", stat.mode);
        // All characters should be octal digits
        assert!(
            stat.mode.chars().all(|c| c.is_ascii_digit() && c < '8'),
            "mode should be octal digits: {}",
            stat.mode
        );
    }

    #[tokio::test]
    async fn stat_nonexistent_returns_error() {
        let result = stat_file("/tmp/ix_stat_nonexistent_xyz.txt").await;
        assert!(result.is_err());
    }

    #[tokio::test]
    async fn list_dir_returns_entries_sorted() {
        let dir = TempDir::new().unwrap();
        // Create files in non-alphabetical order
        std::fs::write(dir.path().join("zebra.txt"), b"").unwrap();
        std::fs::write(dir.path().join("apple.txt"), b"").unwrap();
        std::fs::write(dir.path().join("mango.txt"), b"").unwrap();

        let entries = list_dir(dir.path().to_str().unwrap()).await.unwrap();
        let names: Vec<&str> = entries.iter().map(|e| e.name.as_str()).collect();

        assert_eq!(names, vec!["apple.txt", "mango.txt", "zebra.txt"]);
    }

    #[tokio::test]
    async fn list_dir_distinguishes_files_and_dirs() {
        let dir = TempDir::new().unwrap();
        std::fs::write(dir.path().join("a_file.txt"), b"content").unwrap();
        std::fs::create_dir(dir.path().join("a_subdir")).unwrap();

        let entries = list_dir(dir.path().to_str().unwrap()).await.unwrap();

        let file_entry = entries.iter().find(|e| e.name == "a_file.txt").unwrap();
        assert_eq!(file_entry.entry_type, "file");

        let dir_entry = entries.iter().find(|e| e.name == "a_subdir").unwrap();
        assert_eq!(dir_entry.entry_type, "dir");
    }

    #[tokio::test]
    async fn list_dir_nonexistent_returns_error() {
        let result = list_dir("/tmp/ix_list_nonexistent_xyz").await;
        assert!(result.is_err());
    }

    #[tokio::test]
    async fn list_dir_file_sizes_are_correct() {
        let dir = TempDir::new().unwrap();
        let content = b"exactly ten!"; // 12 bytes
        std::fs::write(dir.path().join("sized.txt"), content).unwrap();

        let entries = list_dir(dir.path().to_str().unwrap()).await.unwrap();
        let entry = entries.iter().find(|e| e.name == "sized.txt").unwrap();
        assert_eq!(entry.size, content.len() as u64);
    }
}
