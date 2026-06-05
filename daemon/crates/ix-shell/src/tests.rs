#[cfg(test)]
mod tests {
    use std::time::Duration;

    use ix_core::sse::test_channel;
    use ix_core::types::ShellRequest;
    use tokio::sync::mpsc;
    use tokio::time::timeout;

    use crate::exec::execute_shell;
    use crate::signal::kill_process_group;

    // ---------------------------------------------------------------------------
    // Test helper: run execute_shell and collect SSE events
    // ---------------------------------------------------------------------------

    /// Collect all `(event_name, data_json)` pairs from the receiver until it closes.
    async fn collect_from_rx(mut rx: mpsc::Receiver<(String, String)>) -> Vec<(String, String)> {
        let mut events = Vec::new();
        while let Some(pair) = rx.recv().await {
            events.push(pair);
        }
        events
    }

    /// Run `execute_shell` with the given request and return all SSE events as
    /// `(event_name, data_json_string)` pairs.
    async fn run_and_collect(req: ShellRequest) -> Vec<(String, String)> {
        let (sender, rx) = test_channel(64);
        // Run execute_shell; it drops `sender` when done, closing the raw channel,
        // which lets the bridge task drain and close the named channel.
        execute_shell(req, sender).await;
        // Give the bridge task a moment to drain any remaining events.
        tokio::time::sleep(Duration::from_millis(50)).await;
        collect_from_rx(rx).await
    }

    // ---------------------------------------------------------------------------
    // Convenience accessors
    // ---------------------------------------------------------------------------

    fn events_of_type<'a>(events: &'a [(String, String)], kind: &str) -> Vec<&'a str> {
        events
            .iter()
            .filter(|(name, _)| name == kind)
            .map(|(_, data)| data.as_str())
            .collect()
    }

    fn complete_event(events: &[(String, String)]) -> Option<serde_json::Value> {
        events
            .iter()
            .find(|(name, _)| name == "complete")
            .and_then(|(_, data)| serde_json::from_str(data).ok())
    }

    fn error_event(events: &[(String, String)]) -> Option<serde_json::Value> {
        events
            .iter()
            .find(|(name, _)| name == "error")
            .and_then(|(_, data)| serde_json::from_str(data).ok())
    }

    // ---------------------------------------------------------------------------
    // exec.rs tests
    // ---------------------------------------------------------------------------

    #[tokio::test]
    async fn test_echo_returns_stdout_and_exit_zero() {
        let req = ShellRequest {
            command: "echo hello".to_string(),
            cwd: None,
            timeout: None,
            session_id: None,
        };
        let events = run_and_collect(req).await;

        let stdout = events_of_type(&events, "stdout");
        assert!(
            stdout.iter().any(|d| d.contains("hello")),
            "expected stdout containing 'hello', got: {:?}",
            stdout
        );

        let complete = complete_event(&events).expect("missing complete event");
        assert_eq!(complete["exit_code"], 0);
    }

    #[tokio::test]
    async fn test_stderr_output_sends_stderr_events() {
        let req = ShellRequest {
            command: "echo err_msg >&2".to_string(),
            cwd: None,
            timeout: None,
            session_id: None,
        };
        let events = run_and_collect(req).await;

        let stderr = events_of_type(&events, "stderr");
        assert!(
            stderr.iter().any(|d| d.contains("err_msg")),
            "expected stderr containing 'err_msg', got: {:?}",
            stderr
        );
    }

    #[tokio::test]
    async fn test_nonzero_exit_code_captured() {
        let req = ShellRequest {
            command: "bash -c 'exit 42'".to_string(),
            cwd: None,
            timeout: None,
            session_id: None,
        };
        let events = run_and_collect(req).await;

        let complete = complete_event(&events).expect("missing complete event");
        assert_eq!(complete["exit_code"], 42, "expected exit code 42");
    }

    #[tokio::test]
    async fn test_custom_working_directory() {
        let tmp = tempfile::tempdir().expect("failed to create tempdir");
        let tmp_path = tmp.path().to_str().unwrap().to_string();

        let req = ShellRequest {
            command: "pwd".to_string(),
            cwd: Some(tmp_path.clone()),
            timeout: None,
            session_id: None,
        };
        let events = run_and_collect(req).await;

        let stdout = events_of_type(&events, "stdout");
        let canon_tmp = std::fs::canonicalize(&tmp_path).unwrap_or_else(|_| tmp.path().into());

        let found = stdout.iter().any(|d| {
            // data is JSON: {"text":"..."}
            let v: serde_json::Value = serde_json::from_str(d).unwrap_or_default();
            let text = v["text"].as_str().unwrap_or("").trim();
            let canon =
                std::fs::canonicalize(text).unwrap_or_else(|_| std::path::PathBuf::from(text));
            canon == canon_tmp
        });

        assert!(
            found,
            "expected pwd output matching '{tmp_path}', got: {:?}",
            stdout
        );
    }

    #[tokio::test]
    async fn test_timeout_kills_process() {
        let req = ShellRequest {
            command: "sleep 999".to_string(),
            cwd: None,
            timeout: Some(1),
            session_id: None,
        };

        // Allow up to 15 s total (1 s timeout + process teardown overhead).
        let events = timeout(Duration::from_secs(15), run_and_collect(req))
            .await
            .expect("test itself timed out — timeout feature may be broken");

        let err = error_event(&events).expect("expected an error event for timed-out command");
        let text = err["text"].as_str().unwrap_or("");
        assert!(
            text.contains("timed out"),
            "expected 'timed out' in error text, got: {text}"
        );
    }

    #[tokio::test]
    async fn test_empty_command_produces_complete_or_error() {
        // An empty string passed to `bash -l -c ""` exits 0 with no output — that
        // is bash's standard behaviour.  We just assert no panic and that a
        // terminal event (complete or error) is present.
        let req = ShellRequest {
            command: String::new(),
            cwd: None,
            timeout: None,
            session_id: None,
        };
        let events = run_and_collect(req).await;
        assert!(
            complete_event(&events).is_some() || error_event(&events).is_some(),
            "expected either complete or error event for empty command, got: {:?}",
            events
        );
    }

    #[tokio::test]
    async fn test_multiline_output_sends_multiple_stdout_events() {
        let req = ShellRequest {
            command: "printf 'line1\nline2\nline3\n'".to_string(),
            cwd: None,
            timeout: None,
            session_id: None,
        };
        let events = run_and_collect(req).await;

        let stdout = events_of_type(&events, "stdout");
        assert!(
            stdout.len() >= 3,
            "expected at least 3 stdout events, got {}: {:?}",
            stdout.len(),
            stdout
        );
    }

    #[tokio::test]
    async fn test_stdout_and_stderr_interleaved() {
        // Write to both stdout and stderr in the same command.
        let req = ShellRequest {
            command: "echo out1; echo err1 >&2; echo out2; echo err2 >&2".to_string(),
            cwd: None,
            timeout: None,
            session_id: None,
        };
        let events = run_and_collect(req).await;

        let stdout = events_of_type(&events, "stdout");
        let stderr = events_of_type(&events, "stderr");

        assert!(
            stdout.iter().any(|d| d.contains("out1") || d.contains("out2")),
            "expected stdout events with out1/out2, got: {:?}",
            stdout
        );
        assert!(
            stderr.iter().any(|d| d.contains("err1") || d.contains("err2")),
            "expected stderr events with err1/err2, got: {:?}",
            stderr
        );
    }

    // ---------------------------------------------------------------------------
    // signal.rs tests
    // ---------------------------------------------------------------------------

    #[tokio::test]
    async fn test_kill_process_group_terminates_process() {
        use std::process::Stdio;
        use tokio::process::Command;

        // Spawn a long-running sleep process in its own process group, using
        // `pre_exec` to call setpgid(0, 0) inside the child before exec —
        // the same technique `execute_shell` uses.
        let mut child = unsafe {
            Command::new("sleep")
                .arg("999")
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .pre_exec(|| {
                    nix::unistd::setpgid(
                        nix::unistd::Pid::from_raw(0),
                        nix::unistd::Pid::from_raw(0),
                    )
                    .map_err(|e| std::io::Error::from_raw_os_error(e as i32))?;
                    Ok(())
                })
                .spawn()
                .expect("failed to spawn sleep process")
        };

        // The child's PID == its PGID after setpgid(0, 0).
        let pid = child.id().expect("child has no PID") as i32;

        // Send SIGTERM (and SIGKILL after 5 s grace period) to the process group.
        kill_process_group(pid).await;

        // After kill_process_group returns the process has been SIGKILLed.
        let result = child.wait().await.expect("wait() failed");
        assert!(
            !result.success(),
            "expected non-zero exit after SIGKILL, got: {:?}",
            result
        );
    }

    // ---------------------------------------------------------------------------
    // session.rs tests
    // ---------------------------------------------------------------------------

    use crate::session::SessionManager;
    use std::sync::Arc;

    fn session_req(sid: &str, command: &str, timeout: Option<u64>) -> ShellRequest {
        // Build via serde so the struct literal stays in one place.
        serde_json::from_value(serde_json::json!({
            "command": command,
            "session_id": sid,
            "timeout": timeout,
        }))
        .unwrap()
    }

    async fn run_in_session(mgr: &Arc<SessionManager>, req: ShellRequest) -> Vec<(String, String)> {
        let (sender, rx) = test_channel(64);
        mgr.execute(req, sender).await;
        tokio::time::sleep(Duration::from_millis(50)).await;
        collect_from_rx(rx).await
    }

    fn stdout_text(events: &[(String, String)]) -> String {
        events_of_type(events, "stdout")
            .iter()
            .map(|d| {
                serde_json::from_str::<serde_json::Value>(d).unwrap()["text"]
                    .as_str()
                    .unwrap()
                    .to_string()
            })
            .collect::<Vec<_>>()
            .join("\n")
    }

    #[tokio::test]
    async fn session_state_persists_across_commands() {
        let mgr = SessionManager::new_shared();
        run_in_session(&mgr, session_req("s1", "X=42", None)).await;
        let events = run_in_session(&mgr, session_req("s1", "echo $X", None)).await;
        assert_eq!(stdout_text(&events), "42");
        let complete = complete_event(&events).expect("complete event");
        assert_eq!(complete["exit_code"], 0);
    }

    #[tokio::test]
    async fn sessions_are_isolated_from_each_other() {
        let mgr = SessionManager::new_shared();
        run_in_session(&mgr, session_req("a", "X=aaa", None)).await;
        let events = run_in_session(&mgr, session_req("b", "echo \"X=[$X]\"", None)).await;
        assert_eq!(stdout_text(&events), "X=[]");
    }

    #[tokio::test]
    async fn session_reports_nonzero_exit_code() {
        let mgr = SessionManager::new_shared();
        let events = run_in_session(&mgr, session_req("s1", "false", None)).await;
        let complete = complete_event(&events).expect("complete event");
        assert_eq!(complete["exit_code"], 1);
    }

    #[tokio::test]
    async fn session_captures_stderr() {
        let mgr = SessionManager::new_shared();
        let events = run_in_session(&mgr, session_req("s1", "echo oops >&2", None)).await;
        let errs = events_of_type(&events, "stderr");
        assert!(
            errs.iter().any(|d| d.contains("oops")),
            "stderr missing: {events:?}"
        );
    }

    #[tokio::test]
    async fn session_handles_output_without_trailing_newline() {
        let mgr = SessionManager::new_shared();
        let events = run_in_session(&mgr, session_req("s1", "printf no-newline", None)).await;
        assert_eq!(stdout_text(&events), "no-newline");
    }

    #[tokio::test]
    async fn session_timeout_kills_and_recreates() {
        let mgr = SessionManager::new_shared();
        let events = run_in_session(&mgr, session_req("s1", "sleep 5", Some(1))).await;
        assert!(error_event(&events).is_some(), "expected timeout error");
        // Session must be recreated transparently on the next command.
        let events = run_in_session(&mgr, session_req("s1", "echo back", None)).await;
        assert_eq!(stdout_text(&events), "back");
    }

    #[tokio::test]
    async fn session_survives_user_exit_by_recreating() {
        let mgr = SessionManager::new_shared();
        run_in_session(&mgr, session_req("s1", "exit 0", None)).await;
        let events = run_in_session(&mgr, session_req("s1", "echo alive", None)).await;
        assert_eq!(stdout_text(&events), "alive");
    }

    #[tokio::test]
    async fn session_is_fast_after_first_command() {
        let mgr = SessionManager::new_shared();
        run_in_session(&mgr, session_req("s1", "true", None)).await; // pays bash -l once
        let t = std::time::Instant::now();
        let (sender, rx) = test_channel(64);
        mgr.execute(session_req("s1", "echo hot", None), sender).await;
        let elapsed = t.elapsed();
        drop(rx);
        // In-guest persistent round-trip must stay well below the ~18 ms
        // fork+exec it guards against; 15 ms keeps that margin while tolerating
        // loaded/contended CI runners that briefly stall the reactor.
        assert!(
            elapsed < Duration::from_millis(15),
            "session round-trip took {elapsed:?}"
        );
    }
}
