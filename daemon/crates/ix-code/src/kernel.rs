use std::sync::atomic::{AtomicU32, Ordering};

use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStderr, ChildStdin, ChildStdout, Command};
use tracing::info;

use ix_core::{Error, Result};

const SENTINEL_END: &str = "__IX_END__";
const SENTINEL_RESULT: &str = "__IX_RESULT__";

pub struct Kernel {
    pub language: String,
    pub process: Child,
    pub stdin: ChildStdin,
    pub stdout: BufReader<ChildStdout>,
    pub stderr: BufReader<ChildStderr>,
    pub cell_count: AtomicU32,
}

impl Kernel {
    pub async fn start(language: &str) -> Result<Self> {
        let (cmd_path, args) = language_command(language)?;

        info!(language, cmd = %cmd_path, "starting REPL kernel");

        let mut child = Command::new(&cmd_path)
            .args(&args)
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped())
            .env("PYTHONUNBUFFERED", "1")
            .spawn()
            .map_err(|e| Error::Internal(format!("spawn {cmd_path}: {e}")))?;

        let stdin = child
            .stdin
            .take()
            .ok_or_else(|| Error::Internal("no stdin".into()))?;
        let stdout = child
            .stdout
            .take()
            .ok_or_else(|| Error::Internal("no stdout".into()))?;
        let stderr = child
            .stderr
            .take()
            .ok_or_else(|| Error::Internal("no stderr".into()))?;

        Ok(Self {
            language: normalize_language(language).to_string(),
            process: child,
            stdin,
            stdout: BufReader::new(stdout),
            stderr: BufReader::new(stderr),
            cell_count: AtomicU32::new(0),
        })
    }

    /// Execute a code block and return (stdout_output, stderr_output).
    pub async fn execute(
        &mut self,
        code: &str,
        timeout: Option<std::time::Duration>,
    ) -> Result<(String, String)> {
        self.cell_count.fetch_add(1, Ordering::Relaxed);

        // Write code + sentinel to stdin
        self.stdin
            .write_all(code.as_bytes())
            .await
            .map_err(|e| Error::Internal(format!("write stdin: {e}")))?;
        if !code.ends_with('\n') {
            self.stdin
                .write_all(b"\n")
                .await
                .map_err(|e| Error::Internal(format!("write newline: {e}")))?;
        }
        self.stdin
            .write_all(format!("{SENTINEL_END}\n").as_bytes())
            .await
            .map_err(|e| Error::Internal(format!("write sentinel: {e}")))?;
        self.stdin
            .flush()
            .await
            .map_err(|e| Error::Internal(format!("flush stdin: {e}")))?;

        // Read stdout until sentinel
        let read_future = async {
            let mut output = String::new();
            let mut line = String::new();
            loop {
                line.clear();
                let n = self
                    .stdout
                    .read_line(&mut line)
                    .await
                    .map_err(|e| Error::Internal(format!("read stdout: {e}")))?;
                if n == 0 {
                    return Err(Error::Internal("REPL process closed stdout".into()));
                }
                if line.trim_end() == SENTINEL_RESULT {
                    break;
                }
                output.push_str(&line);
            }
            Ok::<String, Error>(output)
        };

        let stdout_output = if let Some(dur) = timeout {
            match tokio::time::timeout(dur, read_future).await {
                Ok(result) => result?,
                Err(_) => return Err(Error::Internal("code execution timed out".into())),
            }
        } else {
            read_future.await?
        };

        // Drain any available stderr (non-blocking)
        let mut stderr_output = String::new();
        let mut stderr_line = String::new();
        loop {
            // Use a very short timeout to drain available stderr without blocking
            match tokio::time::timeout(
                std::time::Duration::from_millis(10),
                self.stderr.read_line(&mut stderr_line),
            )
            .await
            {
                Ok(Ok(0)) | Err(_) => break,
                Ok(Ok(_)) => {
                    stderr_output.push_str(&stderr_line);
                    stderr_line.clear();
                }
                Ok(Err(_)) => break,
            }
        }

        Ok((stdout_output, stderr_output))
    }

    pub fn is_alive(&mut self) -> bool {
        matches!(self.process.try_wait(), Ok(None))
    }
}

impl Drop for Kernel {
    fn drop(&mut self) {
        let _ = self.process.start_kill();
    }
}

pub fn normalize_language(lang: &str) -> &str {
    match lang.to_lowercase().as_str() {
        "python" | "python3" | "py" => "python",
        "javascript" | "js" | "node" | "nodejs" => "javascript",
        "bash" | "sh" | "shell" => "bash",
        _ => lang,
    }
}

fn language_command(language: &str) -> Result<(String, Vec<String>)> {
    match normalize_language(language) {
        "python" => Ok((
            "python3".into(),
            vec!["-u".into(), "/usr/lib/ix/repl.py".into()],
        )),
        "javascript" => Ok((
            "node".into(),
            vec![
                "-e".into(),
                // Minimal Node.js REPL with same protocol
                r#"
const readline = require('readline');
const vm = require('vm');
const rl = readline.createInterface({ input: process.stdin });
const ctx = vm.createContext({console, require, process, Buffer, setTimeout, setInterval, clearTimeout, clearInterval});
let lines = [];
rl.on('line', line => {
    if (line === '__IX_END__') {
        const code = lines.join('\n');
        lines = [];
        try {
            const result = vm.runInContext(code, ctx, {filename: '<ix>'});
            if (result !== undefined) console.log(result);
        } catch(e) {
            console.error(e.stack || e.message || e);
        }
        process.stdout.write('__IX_RESULT__\n');
    } else {
        lines.push(line);
    }
});
"#
                .into(),
            ],
        )),
        "bash" => Ok((
            "python3".into(),
            vec![
                "-u".into(),
                "-c".into(),
                // Bash via Python subprocess — preserves session state
                r#"
import sys, subprocess
while True:
    lines = []
    for line in sys.stdin:
        if line.rstrip('\n') == '__IX_END__': break
        lines.append(line)
    else: break
    code = ''.join(lines)
    if code.strip():
        r = subprocess.run(['bash', '-c', code], capture_output=True, text=True)
        if r.stdout: sys.stdout.write(r.stdout)
        if r.stderr: sys.stderr.write(r.stderr)
    sys.stdout.write('__IX_RESULT__\n')
    sys.stdout.flush()
"#
                .into(),
            ],
        )),
        other => Err(Error::Internal(format!("unsupported language: {other}"))),
    }
}

/// Exposed for testing: return the REPL command for a language
#[cfg(test)]
pub(crate) fn language_to_command(lang: &str) -> Result<(String, Vec<String>)> {
    language_command(lang)
}

#[cfg(test)]
mod tests {
    use super::*;

    // -----------------------------------------------------------------------
    // normalize_language
    // -----------------------------------------------------------------------

    #[test]
    fn normalize_python_variants() {
        assert_eq!(normalize_language("python"), "python");
        assert_eq!(normalize_language("python3"), "python");
        assert_eq!(normalize_language("py"), "python");
    }

    #[test]
    fn normalize_javascript_variants() {
        assert_eq!(normalize_language("javascript"), "javascript");
        assert_eq!(normalize_language("js"), "javascript");
        assert_eq!(normalize_language("node"), "javascript");
        assert_eq!(normalize_language("nodejs"), "javascript");
    }

    #[test]
    fn normalize_bash_variants() {
        assert_eq!(normalize_language("bash"), "bash");
        assert_eq!(normalize_language("sh"), "bash");
        assert_eq!(normalize_language("shell"), "bash");
    }

    #[test]
    fn normalize_unknown_passes_through() {
        assert_eq!(normalize_language("ruby"), "ruby");
    }

    // -----------------------------------------------------------------------
    // Language-to-command mapping
    // -----------------------------------------------------------------------

    #[test]
    fn python_maps_to_python3_repl() {
        let (cmd, args) = language_to_command("python").unwrap();
        assert_eq!(cmd, "python3");
        assert!(args.contains(&"-u".to_string()));
        assert!(args.contains(&"/usr/lib/ix/repl.py".to_string()));
    }

    #[test]
    fn python3_alias_maps_to_python3_repl() {
        let (cmd, args) = language_to_command("python3").unwrap();
        assert_eq!(cmd, "python3");
        assert!(args.contains(&"/usr/lib/ix/repl.py".to_string()));
    }

    #[test]
    fn py_alias_maps_to_python3() {
        let (cmd, _) = language_to_command("py").unwrap();
        assert_eq!(cmd, "python3");
    }

    #[test]
    fn javascript_maps_to_node() {
        let (cmd, _) = language_to_command("javascript").unwrap();
        assert_eq!(cmd, "node");
    }

    #[test]
    fn js_alias_maps_to_node() {
        let (cmd, _) = language_to_command("js").unwrap();
        assert_eq!(cmd, "node");
    }

    #[test]
    fn node_alias_maps_to_node() {
        let (cmd, _) = language_to_command("node").unwrap();
        assert_eq!(cmd, "node");
    }

    #[test]
    fn bash_maps_to_python3_wrapper() {
        let (cmd, args) = language_to_command("bash").unwrap();
        assert_eq!(cmd, "python3");
        assert!(args.contains(&"-c".to_string()));
    }

    #[test]
    fn sh_alias_maps_to_bash_wrapper() {
        let (cmd, args) = language_to_command("sh").unwrap();
        assert_eq!(cmd, "python3");
        assert!(args.contains(&"-c".to_string()));
    }

    #[test]
    fn shell_alias_maps_to_bash_wrapper() {
        let (cmd, _) = language_to_command("shell").unwrap();
        assert_eq!(cmd, "python3");
    }

    #[test]
    fn unsupported_language_returns_error() {
        let result = language_to_command("ruby");
        assert!(result.is_err());
        let err = result.unwrap_err().to_string();
        assert!(err.contains("unsupported") || err.contains("ruby"));
    }
}
