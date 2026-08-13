use crate::stat::{format_system_time, unix_nanos};
use ix_core::{
    types::{
        GlobEntry, GlobRequest, GlobResult, GrepMatch, GrepRequest, GrepResult, TreeRequest,
        TreeResult,
    },
    Error, Result,
};
use regex::Regex;
use std::path::Path;
use tokio::process::Command;
use walkdir::WalkDir;

const DEFAULT_GLOB_LIMIT: usize = 1000;
const DEFAULT_GREP_LIMIT: usize = 100;
const DEFAULT_TREE_DEPTH: usize = 3;

// ---------------------------------------------------------------------------
// Glob
// ---------------------------------------------------------------------------

pub async fn glob_files(req: GlobRequest) -> Result<GlobResult> {
    let limit = req.limit.unwrap_or(DEFAULT_GLOB_LIMIT);
    let excludes: Vec<String> = req
        .exclude
        .unwrap_or_else(|| vec![".git".to_string()]);

    // Try fd / fdfind first.
    if let Some(result) = try_fd_glob(&req.pattern, &req.path, &excludes, limit).await {
        return Ok(result);
    }

    // Fallback: walkdir + glob pattern matching.
    native_glob(&req.pattern, &req.path, &excludes, limit)
}

async fn try_fd_glob(
    pattern: &str,
    path: &str,
    excludes: &[String],
    limit: usize,
) -> Option<GlobResult> {
    // Prefer fd over fdfind.
    let bin = if which("fd") { "fd" } else if which("fdfind") { "fdfind" } else { return None; };

    let mut cmd = Command::new(bin);
    cmd.arg("--glob").arg(pattern).arg(path);
    for ex in excludes {
        cmd.arg("--exclude").arg(ex);
    }
    cmd.arg("--max-results").arg(limit.to_string());

    let out = cmd.output().await.ok()?;
    if !out.status.success() {
        return None;
    }

    let stdout = String::from_utf8_lossy(&out.stdout);
    let files: Vec<String> = stdout
        .lines()
        .map(|l| l.to_string())
        .collect();
    let truncated = files.len() >= limit;
    let entries = describe(&files);
    Some(GlobResult { files, entries, truncated })
}

fn native_glob(
    pattern: &str,
    base: &str,
    excludes: &[String],
    limit: usize,
) -> Result<GlobResult> {
    use glob::Pattern;

    let pat = Pattern::new(pattern)
        .map_err(|e| Error::BadRequest(format!("invalid glob pattern: {}", e)))?;

    let mut files = Vec::new();
    let mut truncated = false;

    let walker = WalkDir::new(base).into_iter().filter_entry(|e| {
        if e.depth() > 0 && e.file_type().is_dir() {
            if let Some(name) = e.file_name().to_str() {
                return !excludes.iter().any(|ex| ex == name);
            }
        }
        true
    });

    'walk: for entry in walker.filter_map(|e| e.ok()) {
        if entry.file_type().is_file() {
            let file_name = entry.file_name().to_string_lossy();
            if pat.matches(&file_name) {
                if files.len() >= limit {
                    truncated = true;
                    break 'walk;
                }
                files.push(entry.path().display().to_string());
            }
        }
    }

    let entries = describe(&files);
    Ok(GlobResult { files, entries, truncated })
}

/// Stat every hit so the caller gets size + mtime alongside the names.
///
/// A path that will not stat is skipped rather than dropped from `files` or
/// escalated into an error: it was on disk a moment ago, the glob is racing
/// whatever is writing to the workspace, and one unreadable path must not cost
/// the caller the other 999. Leaving it in `files` with no entry says "here,
/// but undescribed", which a caller can act on; removing it would say "gone",
/// which is a different and worse claim.
fn describe(files: &[String]) -> Vec<GlobEntry> {
    files
        .iter()
        .filter_map(|path| {
            let meta = std::fs::metadata(path).ok()?;
            let modified = meta.modified().ok()?;
            Some(GlobEntry {
                path: path.clone(),
                size: meta.len(),
                mod_time: format_system_time(modified),
                mod_time_nanos: unix_nanos(modified),
            })
        })
        .collect()
}

// ---------------------------------------------------------------------------
// Grep
// ---------------------------------------------------------------------------

pub async fn grep_files(req: GrepRequest) -> Result<GrepResult> {
    let limit = req.limit.unwrap_or(DEFAULT_GREP_LIMIT);
    let context = req.context.unwrap_or(0);

    // Try ripgrep first.
    if let Some(result) = try_rg_grep(&req.pattern, &req.path, req.glob.as_deref(), context, limit).await {
        return Ok(result);
    }

    // Fallback: native.
    native_grep(&req.pattern, &req.path, req.glob.as_deref(), context, limit).await
}

async fn try_rg_grep(
    pattern: &str,
    path: &str,
    glob: Option<&str>,
    context: usize,
    limit: usize,
) -> Option<GrepResult> {
    if !which("rg") {
        return None;
    }

    let mut cmd = Command::new("rg");
    cmd.arg("--json")
        .arg("--max-count")
        .arg("1")
        .arg(format!("--before-context={}", context))
        .arg(format!("--after-context={}", context));

    if let Some(g) = glob {
        cmd.arg("--glob").arg(g);
    }

    cmd.arg(pattern).arg(path);

    let out = cmd.output().await.ok()?;
    // rg exits 1 when no matches — that's fine.
    let stdout = String::from_utf8_lossy(&out.stdout);

    let mut matches: Vec<GrepMatch> = Vec::new();

    // rg's --json stream interleaves "context" events with the "match" event
    // they surround: before-context lines arrive first, then the match, then
    // after-context lines. `pending_before` holds context seen ahead of the
    // next match; once that match is pushed, `open_for_context` routes any
    // further context lines (up to the next "begin"/"end") onto its
    // `context_after` instead of starting a new before-buffer.
    let mut pending_before: Vec<String> = Vec::new();
    let mut open_for_context = false;

    for line in stdout.lines() {
        let v: serde_json::Value = serde_json::from_str(line).ok()?;
        match v["type"].as_str() {
            Some("match") => {
                let data = &v["data"];
                let file_path = data["path"]["text"].as_str().unwrap_or("").to_string();
                let lineno = data["line_number"].as_u64().unwrap_or(0) as usize;
                let content = data["lines"]["text"]
                    .as_str()
                    .unwrap_or("")
                    .trim_end_matches('\n')
                    .to_string();

                matches.push(GrepMatch {
                    path: file_path,
                    line: lineno,
                    content,
                    context_before: std::mem::take(&mut pending_before),
                    context_after: Vec::new(),
                });
                open_for_context = true;
            }
            Some("context") => {
                let text = v["data"]["lines"]["text"]
                    .as_str()
                    .unwrap_or("")
                    .trim_end_matches('\n')
                    .to_string();
                if open_for_context {
                    if let Some(m) = matches.last_mut() {
                        m.context_after.push(text);
                    }
                } else {
                    pending_before.push(text);
                }
            }
            Some("begin") | Some("end") => {
                // File boundary: a stray before-context buffer belongs to no
                // match (rg only emits context around one) and after-context
                // stops here, since --max-count=1 means each file's match, if
                // any, has already been claimed.
                pending_before.clear();
                open_for_context = false;
            }
            _ => {}
        }
    }

    // rg already ran to completion by the time `out` was captured above, so
    // trimming here (rather than breaking the loop early) costs nothing and
    // guarantees every match kept has its full context, matching how
    // `native_grep` computes context at push time.
    let truncated = matches.len() > limit;
    matches.truncate(limit);

    Some(GrepResult { matches, truncated })
}

async fn native_grep(
    pattern: &str,
    base: &str,
    glob_filter: Option<&str>,
    context: usize,
    limit: usize,
) -> Result<GrepResult> {
    use glob::Pattern;

    let re = Regex::new(pattern)
        .map_err(|e| Error::BadRequest(format!("invalid regex: {}", e)))?;

    let glob_pat: Option<Pattern> = glob_filter
        .map(|g| Pattern::new(g).map_err(|e| Error::BadRequest(format!("invalid glob: {}", e))))
        .transpose()?;

    let mut matches = Vec::new();
    let mut truncated = false;

    'walk: for entry in WalkDir::new(base).into_iter().filter_map(|e| e.ok()) {
        if !entry.file_type().is_file() {
            continue;
        }

        // Apply glob filter on filename.
        if let Some(ref pat) = glob_pat {
            let name = entry.file_name().to_string_lossy();
            if !pat.matches(&name) {
                continue;
            }
        }

        let file_path = entry.path().display().to_string();
        let content = match std::fs::read_to_string(entry.path()) {
            Ok(c) => c,
            Err(_) => continue, // skip binary / unreadable files
        };

        let lines: Vec<&str> = content.lines().collect();

        for (i, line) in lines.iter().enumerate() {
            if re.is_match(line) {
                if matches.len() >= limit {
                    truncated = true;
                    break 'walk;
                }

                let before_start = i.saturating_sub(context);
                let context_before: Vec<String> = lines[before_start..i]
                    .iter()
                    .map(|l| l.to_string())
                    .collect();

                let after_end = (i + 1 + context).min(lines.len());
                let context_after: Vec<String> = lines[i + 1..after_end]
                    .iter()
                    .map(|l| l.to_string())
                    .collect();

                matches.push(GrepMatch {
                    path: file_path.clone(),
                    line: i + 1, // 1-based
                    content: line.to_string(),
                    context_before,
                    context_after,
                });
            }
        }
    }

    Ok(GrepResult { matches, truncated })
}

// ---------------------------------------------------------------------------
// Tree
// ---------------------------------------------------------------------------

pub async fn tree(req: TreeRequest) -> Result<TreeResult> {
    let depth = req.depth.unwrap_or(DEFAULT_TREE_DEPTH);
    let excludes: Vec<String> = req.exclude.unwrap_or_else(|| {
        vec![
            ".git".to_string(),
            "node_modules".to_string(),
            "__pycache__".to_string(),
            ".venv".to_string(),
            "vendor".to_string(),
        ]
    });

    // Always count with native walker (tree v2.0+ counts the root dir, giving
    // inconsistent results across environments).
    let mut result = native_tree(&req.path, depth, &excludes)?;

    // Use tree binary for nicer formatted output when available.
    if let Some(tree_str) = try_tree_bin_output(&req.path, depth, &excludes).await {
        result.tree = tree_str;
    }

    Ok(result)
}

async fn try_tree_bin_output(path: &str, depth: usize, excludes: &[String]) -> Option<String> {
    if !which("tree") {
        return None;
    }

    let mut cmd = Command::new("tree");
    cmd.arg("-L")
        .arg(depth.to_string())
        .arg("--dirsfirst");
    if !excludes.is_empty() {
        cmd.arg("-I").arg(excludes.join("|"));
    }
    cmd.arg(path);

    let out = cmd.output().await.ok()?;
    if !out.status.success() {
        return None;
    }

    Some(String::from_utf8_lossy(&out.stdout).to_string())
}

fn native_tree(base: &str, depth: usize, excludes: &[String]) -> Result<TreeResult> {
    let base_path = Path::new(base);
    let mut lines: Vec<String> = Vec::new();
    let mut file_count: usize = 0;
    let mut dir_count: usize = 0;

    lines.push(base_path.display().to_string());

    native_tree_recurse(base_path, 0, depth, excludes, &mut lines, &mut file_count, &mut dir_count);

    lines.push(format!("\n{} directories, {} files", dir_count, file_count));
    Ok(TreeResult {
        tree: lines.join("\n"),
        files: file_count,
        dirs: dir_count,
    })
}

fn native_tree_recurse(
    dir: &Path,
    current_depth: usize,
    max_depth: usize,
    excludes: &[String],
    lines: &mut Vec<String>,
    file_count: &mut usize,
    dir_count: &mut usize,
) {
    if current_depth >= max_depth {
        return;
    }

    let mut entries: Vec<std::fs::DirEntry> = match std::fs::read_dir(dir) {
        Ok(rd) => rd.filter_map(|e| e.ok()).collect(),
        Err(_) => return,
    };

    entries.sort_by_key(|e| {
        let is_file = e.file_type().map(|ft| ft.is_file()).unwrap_or(true);
        (is_file as u8, e.file_name())
    });

    let count = entries.len();
    for (i, entry) in entries.iter().enumerate() {
        let name = entry.file_name();
        let name_str = name.to_string_lossy();

        if excludes.iter().any(|ex| ex.as_str() == name_str) {
            continue;
        }

        let is_last = i == count - 1;
        let connector = if is_last { "└── " } else { "├── " };
        let indent = "│   ".repeat(current_depth);

        let ft = entry.file_type().unwrap();
        if ft.is_dir() {
            *dir_count += 1;
            lines.push(format!("{}{}{}", indent, connector, name_str));
            native_tree_recurse(
                &entry.path(),
                current_depth + 1,
                max_depth,
                excludes,
                lines,
                file_count,
                dir_count,
            );
        } else {
            *file_count += 1;
            lines.push(format!("{}{}{}", indent, connector, name_str));
        }
    }
}

// ---------------------------------------------------------------------------
// Helper
// ---------------------------------------------------------------------------

fn which(bin: &str) -> bool {
    std::process::Command::new("which")
        .arg(bin)
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false)
}

#[cfg(test)]
mod tests {
    use super::*;
    use ix_core::types::{GlobRequest, GrepRequest, TreeRequest};
    use std::io::Write;
    use tempfile::TempDir;

    // -----------------------------------------------------------------------
    // Helpers
    // -----------------------------------------------------------------------

    fn create_file(dir: &TempDir, name: &str, content: &str) -> std::path::PathBuf {
        let path = dir.path().join(name);
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent).unwrap();
        }
        let mut f = std::fs::File::create(&path).unwrap();
        f.write_all(content.as_bytes()).unwrap();
        path
    }

    fn glob_req(pattern: &str, path: &str, exclude: Option<Vec<String>>, limit: Option<usize>) -> GlobRequest {
        GlobRequest {
            pattern: pattern.to_string(),
            path: path.to_string(),
            exclude,
            limit,
        }
    }

    fn grep_req(
        pattern: &str,
        path: &str,
        glob: Option<&str>,
        context: Option<usize>,
        limit: Option<usize>,
    ) -> GrepRequest {
        GrepRequest {
            pattern: pattern.to_string(),
            path: path.to_string(),
            glob: glob.map(|s| s.to_string()),
            context,
            limit,
        }
    }

    fn tree_req(path: &str, depth: Option<usize>, exclude: Option<Vec<String>>) -> TreeRequest {
        TreeRequest {
            path: path.to_string(),
            depth,
            exclude,
        }
    }

    // -----------------------------------------------------------------------
    // Glob tests (native fallback — bypass fd by using a path that avoids fd)
    // -----------------------------------------------------------------------

    #[tokio::test]
    async fn glob_finds_matching_files() {
        let dir = TempDir::new().unwrap();
        create_file(&dir, "foo.rs", "");
        create_file(&dir, "bar.rs", "");
        create_file(&dir, "baz.txt", "");

        let result = glob_files(glob_req("*.rs", dir.path().to_str().unwrap(), None, None))
            .await
            .unwrap();

        assert_eq!(result.files.len(), 2, "expected 2 .rs files, got: {:?}", result.files);
        assert!(result.files.iter().all(|f| f.ends_with(".rs")));
    }

    #[tokio::test]
    async fn glob_respects_exclude() {
        let dir = TempDir::new().unwrap();
        create_file(&dir, "good.rs", "");
        // Create a sub-dir that should be excluded
        let sub = dir.path().join("skip_me");
        std::fs::create_dir_all(&sub).unwrap();
        let mut f = std::fs::File::create(sub.join("hidden.rs")).unwrap();
        f.write_all(b"").unwrap();

        let result = glob_files(glob_req(
            "*.rs",
            dir.path().to_str().unwrap(),
            Some(vec!["skip_me".to_string()]),
            None,
        ))
        .await
        .unwrap();

        // Only good.rs should appear — hidden.rs is in excluded dir
        assert!(
            result.files.iter().all(|f| !f.contains("skip_me")),
            "excluded dir files should not appear: {:?}",
            result.files
        );
        assert!(result.files.iter().any(|f| f.ends_with("good.rs")));
    }

    #[tokio::test]
    async fn glob_truncates_at_limit() {
        let dir = TempDir::new().unwrap();
        for i in 0..10 {
            create_file(&dir, &format!("file{}.rs", i), "");
        }

        let result = glob_files(glob_req("*.rs", dir.path().to_str().unwrap(), None, Some(3)))
            .await
            .unwrap();

        // With native fallback: truncated flag is set when files.len() >= limit
        // The actual files list should not exceed 3 (we break when we hit limit)
        assert!(result.files.len() <= 3);
        assert!(result.truncated);
    }

    #[tokio::test]
    async fn glob_describes_each_file_it_lists() {
        let dir = TempDir::new().unwrap();
        create_file(&dir, "one.rs", "abc"); // 3 bytes
        create_file(&dir, "two.rs", "abcdefgh"); // 8 bytes

        let result = glob_files(glob_req("*.rs", dir.path().to_str().unwrap(), None, None))
            .await
            .unwrap();

        assert_eq!(result.files.len(), 2);
        assert_eq!(result.entries.len(), 2);

        for (name, size) in [("one.rs", 3u64), ("two.rs", 8u64)] {
            let entry = result
                .entries
                .iter()
                .find(|e| e.path.ends_with(name))
                .unwrap_or_else(|| panic!("no entry for {name}: {:?}", result.entries));
            assert_eq!(entry.size, size);
            assert!(
                entry.mod_time.starts_with("20") && entry.mod_time.ends_with('Z'),
                "mod_time should be RFC 3339 UTC: {}",
                entry.mod_time
            );
            assert!(entry.mod_time_nanos > 0);
        }
    }

    #[tokio::test]
    async fn glob_entries_are_matched_to_files_by_path() {
        let dir = TempDir::new().unwrap();
        create_file(&dir, "a.rs", "");
        create_file(&dir, "b.rs", "");
        create_file(&dir, "c.txt", "");

        let result = glob_files(glob_req("*.rs", dir.path().to_str().unwrap(), None, None))
            .await
            .unwrap();

        // `files` keeps its old meaning: every name, nothing else.
        assert_eq!(result.files.len(), 2);
        assert!(result.files.iter().all(|f| f.ends_with(".rs")));

        // `entries` never names a path that isn't in `files`.
        assert!(
            result.entries.iter().all(|e| result.files.contains(&e.path)),
            "entries: {:?} files: {:?}",
            result.entries,
            result.files
        );
    }

    #[tokio::test]
    async fn native_glob_describes_its_hits_too() {
        // glob_files prefers fd wherever it is installed, so the walkdir
        // fallback needs its own call or it goes untested on most machines.
        let dir = TempDir::new().unwrap();
        create_file(&dir, "only.rs", "abcd"); // 4 bytes

        let result = native_glob("*.rs", dir.path().to_str().unwrap(), &[], 100).unwrap();

        assert_eq!(result.files.len(), 1);
        assert_eq!(result.entries.len(), 1);
        assert_eq!(result.entries[0].path, result.files[0]);
        assert_eq!(result.entries[0].size, 4);
        assert!(result.entries[0].mod_time_nanos > 0);
    }

    #[tokio::test]
    async fn describe_skips_what_it_cannot_stat_and_keeps_the_rest() {
        // The delete-during-glob race, forced: a path that was listed and is
        // gone by the time it is stat'd yields no entry, and does not take the
        // rest of the batch with it.
        let dir = TempDir::new().unwrap();
        let real = create_file(&dir, "real.rs", "x");
        let gone = dir.path().join("gone.rs");

        let files = vec![
            gone.to_str().unwrap().to_string(),
            real.to_str().unwrap().to_string(),
        ];
        let entries = describe(&files);

        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].path, real.to_str().unwrap());
        assert_eq!(entries[0].size, 1);
    }

    // -----------------------------------------------------------------------
    // Grep tests
    // -----------------------------------------------------------------------

    #[tokio::test]
    async fn grep_finds_matches_with_correct_line_numbers() {
        let dir = TempDir::new().unwrap();
        create_file(&dir, "a.txt", "alpha\nbeta\ngamma\n");

        let result = grep_files(grep_req("beta", dir.path().to_str().unwrap(), None, None, None))
            .await
            .unwrap();

        assert_eq!(result.matches.len(), 1);
        assert_eq!(result.matches[0].line, 2);
        assert_eq!(result.matches[0].content, "beta");
    }

    #[tokio::test]
    async fn grep_returns_context_lines() {
        // grep_files prefers rg when it's on PATH, falling back to the native
        // walker otherwise — both backends must honor `context` identically,
        // since GrepMatch.context_before/context_after is part of the public
        // contract (ix-core's GrepMatch, mirrored by go-sdk's ContextBefore/
        // ContextAfter and consumed by oasis's file_search tool).
        let dir = TempDir::new().unwrap();
        create_file(&dir, "ctx.txt", "before\nmatch_me\nafter\n");

        let result = grep_files(grep_req(
            "match_me",
            dir.path().to_str().unwrap(),
            None,
            Some(1),
            None,
        ))
        .await
        .unwrap();

        assert_eq!(result.matches.len(), 1);
        let m = &result.matches[0];
        assert_eq!(m.content, "match_me");
        assert_eq!(m.line, 2);
        assert_eq!(m.context_before, vec!["before"]);
        assert_eq!(m.context_after, vec!["after"]);
    }

    #[tokio::test]
    async fn rg_grep_parses_context_from_json_stream() {
        // Exercises try_rg_grep directly (bypassing the native fallback) so
        // this pins the rg --json parsing regardless of which backend
        // grep_files happens to prefer on the machine running the test.
        if !which("rg") {
            eprintln!("rg not on PATH — skipping rg-specific context test");
            return;
        }

        let dir = TempDir::new().unwrap();
        create_file(
            &dir,
            "multi.txt",
            "l1\nl2\nneedle\nl4\nl5\nl6\n", // needle on line 3, context=2
        );
        create_file(&dir, "other.txt", "x\nneedle\ny\n"); // needle on line 2, context=2

        let result = try_rg_grep("needle", dir.path().to_str().unwrap(), None, 2, 100)
            .await
            .expect("rg should have produced a result");

        assert_eq!(result.matches.len(), 2, "expected one match per file: {:?}", result.matches);

        let multi = result
            .matches
            .iter()
            .find(|m| m.path.ends_with("multi.txt"))
            .expect("missing match for multi.txt");
        assert_eq!(multi.line, 3);
        assert_eq!(multi.content, "needle");
        assert_eq!(multi.context_before, vec!["l1", "l2"]);
        assert_eq!(multi.context_after, vec!["l4", "l5"]);

        let other = result
            .matches
            .iter()
            .find(|m| m.path.ends_with("other.txt"))
            .expect("missing match for other.txt");
        assert_eq!(other.line, 2);
        assert_eq!(other.content, "needle");
        // Only one line available on either side even though context=2 was requested.
        assert_eq!(other.context_before, vec!["x"]);
        assert_eq!(other.context_after, vec!["y"]);
    }

    #[tokio::test]
    async fn rg_grep_truncates_to_limit_with_full_context() {
        if !which("rg") {
            eprintln!("rg not on PATH — skipping rg-specific truncation test");
            return;
        }

        let dir = TempDir::new().unwrap();
        for i in 0..5 {
            create_file(&dir, &format!("f{i}.txt"), "before\nneedle\nafter\n");
        }

        let result = try_rg_grep("needle", dir.path().to_str().unwrap(), None, 1, 3)
            .await
            .expect("rg should have produced a result");

        assert_eq!(result.matches.len(), 3);
        assert!(result.truncated);
        // Every kept match still has complete context — truncation trims the
        // match list, it does not cut a match's context short.
        for m in &result.matches {
            assert_eq!(m.context_before, vec!["before"]);
            assert_eq!(m.context_after, vec!["after"]);
        }
    }

    #[tokio::test]
    async fn grep_truncates_at_limit() {
        let dir = TempDir::new().unwrap();
        // 20 separate files, each with exactly one line matching "XMATCH".
        // rg uses --max-count 1 per file so this yields 20 rg matches total;
        // the native fallback also produces 20. Limit to 5 → truncated.
        for i in 0..20 {
            create_file(&dir, &format!("m{:02}.txt", i), &format!("XMATCH line {}\n", i));
        }

        let result = grep_files(grep_req(
            "XMATCH",
            dir.path().to_str().unwrap(),
            None,
            None,
            Some(5),
        ))
        .await
        .unwrap();

        assert!(
            result.matches.len() <= 5,
            "expected at most 5 matches, got {}",
            result.matches.len()
        );
        assert!(result.truncated, "expected truncated=true");
    }

    // -----------------------------------------------------------------------
    // Tree tests
    // -----------------------------------------------------------------------

    #[tokio::test]
    async fn tree_returns_correct_file_and_dir_counts() {
        let dir = TempDir::new().unwrap();
        create_file(&dir, "f1.txt", "");
        create_file(&dir, "f2.txt", "");
        let sub = dir.path().join("subdir");
        std::fs::create_dir_all(&sub).unwrap();
        let mut f = std::fs::File::create(sub.join("f3.txt")).unwrap();
        f.write_all(b"").unwrap();

        let result = tree(tree_req(dir.path().to_str().unwrap(), Some(3), Some(vec![])))
            .await
            .unwrap();

        assert_eq!(result.files, 3);
        assert_eq!(result.dirs, 1);
    }

    #[tokio::test]
    async fn tree_respects_depth_limit() {
        let dir = TempDir::new().unwrap();
        // Depth: dir/a/b/deep.txt — depth=1 should not see deep.txt
        create_file(&dir, "a/b/deep.txt", "");
        create_file(&dir, "shallow.txt", "");

        let result = tree(tree_req(dir.path().to_str().unwrap(), Some(1), Some(vec![])))
            .await
            .unwrap();

        // depth=1: only entries directly under dir are visited
        // "a" directory is counted but its contents are not traversed
        assert!(!result.tree.contains("deep.txt"), "deep.txt should not appear at depth=1");
    }

    #[tokio::test]
    async fn tree_respects_exclude_list() {
        let dir = TempDir::new().unwrap();
        create_file(&dir, "included.txt", "");
        let excluded = dir.path().join("excluded_dir");
        std::fs::create_dir_all(&excluded).unwrap();
        let mut f = std::fs::File::create(excluded.join("hidden.txt")).unwrap();
        f.write_all(b"").unwrap();

        let result = tree(tree_req(
            dir.path().to_str().unwrap(),
            Some(3),
            Some(vec!["excluded_dir".to_string()]),
        ))
        .await
        .unwrap();

        assert!(!result.tree.contains("excluded_dir"), "excluded dir should not appear");
        assert!(!result.tree.contains("hidden.txt"), "files in excluded dir should not appear");
        assert!(result.tree.contains("included.txt"));
    }
}
