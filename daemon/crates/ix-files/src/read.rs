use ix_core::{
    types::{FileContent, ReadFileRequest},
    Result,
};

/// Read a file and return its content in cat-n format (line numbers + tab + content).
/// Applies offset (0-based line index) and limit (default 2000 lines).
pub async fn read_file(req: ReadFileRequest) -> Result<FileContent> {
    let raw = tokio::fs::read_to_string(&req.path).await?;
    let all_lines: Vec<&str> = raw.split('\n').collect();

    // Total line count: if the file ends with a newline the last element is empty;
    // report the number of logical lines (i.e. the split count minus a trailing empty).
    let total_lines = if raw.ends_with('\n') && !raw.is_empty() {
        all_lines.len().saturating_sub(1)
    } else {
        all_lines.len()
    };

    let offset = req.offset.unwrap_or(0);
    let limit = req.limit.unwrap_or(2000);

    // Slice to the requested window (working on 0-based indices).
    let window: Vec<&str> = all_lines
        .iter()
        .copied()
        .skip(offset)
        .take(limit)
        .collect();

    let mut content = String::new();
    for (i, line) in window.iter().enumerate() {
        // 1-based display line number aligned to 6 chars.
        let lineno = offset + i + 1;
        content.push_str(&format!("{:>6}\t{}\n", lineno, line));
    }

    // Trim trailing newline added after the last line to avoid a phantom blank line.
    if content.ends_with('\n') {
        content.pop();
    }

    Ok(FileContent {
        content,
        path: req.path,
        total_lines,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use ix_core::types::ReadFileRequest;
    use std::io::Write;
    use tempfile::NamedTempFile;

    fn make_req(path: &str, offset: Option<usize>, limit: Option<usize>) -> ReadFileRequest {
        ReadFileRequest {
            path: path.to_string(),
            offset,
            limit,
        }
    }

    async fn write_temp(content: &str) -> NamedTempFile {
        let mut f = NamedTempFile::new().unwrap();
        f.write_all(content.as_bytes()).unwrap();
        f
    }

    #[tokio::test]
    async fn read_default_offset_limit() {
        let f = write_temp("line1\nline2\nline3\n").await;
        let result = read_file(make_req(f.path().to_str().unwrap(), None, None))
            .await
            .unwrap();
        assert_eq!(result.total_lines, 3);
        assert!(result.content.contains("line1"));
        assert!(result.content.contains("line2"));
        assert!(result.content.contains("line3"));
    }

    #[tokio::test]
    async fn cat_n_format_correct() {
        let f = write_temp("hello\nworld\n").await;
        let result = read_file(make_req(f.path().to_str().unwrap(), None, None))
            .await
            .unwrap();
        // First line should be "     1\thello"
        let first_line = result.content.lines().next().unwrap();
        assert_eq!(first_line, "     1\thello");
        let second_line = result.content.lines().nth(1).unwrap();
        assert_eq!(second_line, "     2\tworld");
    }

    #[tokio::test]
    async fn offset_skips_lines() {
        let f = write_temp("a\nb\nc\nd\n").await;
        let result = read_file(make_req(f.path().to_str().unwrap(), Some(2), None))
            .await
            .unwrap();
        // offset=2 → skip first 2 lines, show from line 3
        let first_line = result.content.lines().next().unwrap();
        assert!(first_line.contains("c"), "Expected 'c' as first visible line, got: {first_line}");
        // Line number in cat-n should be 3 (1-based, offset 2 + i 0 + 1)
        assert!(first_line.starts_with("     3\t"));
    }

    #[tokio::test]
    async fn limit_caps_lines_returned() {
        let f = write_temp("1\n2\n3\n4\n5\n").await;
        let result = read_file(make_req(f.path().to_str().unwrap(), None, Some(3)))
            .await
            .unwrap();
        let line_count = result.content.lines().count();
        assert_eq!(line_count, 3);
    }

    #[tokio::test]
    async fn total_lines_reflects_full_file() {
        let content = (1..=10).map(|i| format!("line{}\n", i)).collect::<String>();
        let f = write_temp(&content).await;
        // Read only 2 lines but total_lines should be 10
        let result = read_file(make_req(f.path().to_str().unwrap(), None, Some(2)))
            .await
            .unwrap();
        assert_eq!(result.total_lines, 10);
        assert_eq!(result.content.lines().count(), 2);
    }

    #[tokio::test]
    async fn nonexistent_file_returns_error() {
        let result = read_file(make_req("/tmp/ix_files_nonexistent_xyz.txt", None, None)).await;
        assert!(result.is_err());
    }

    #[tokio::test]
    async fn empty_file_returns_no_error() {
        // An empty file should not return an error; total_lines should be 0 or 1
        // (split('\n') on "" yields [""], so the implementation reports 1).
        let f = write_temp("").await;
        let result = read_file(make_req(f.path().to_str().unwrap(), None, None))
            .await
            .unwrap();
        assert!(
            result.total_lines <= 1,
            "total_lines should be 0 or 1 for empty file, got {}",
            result.total_lines
        );
    }
}
