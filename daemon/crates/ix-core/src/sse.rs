use axum::response::sse::{Event, KeepAlive, KeepAliveStream, Sse};
use std::convert::Infallible;
use std::time::Duration;
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;

pub type SseResponse =
    Sse<KeepAliveStream<ReceiverStream<std::result::Result<Event, Infallible>>>>;

/// Create a `(SseSender, Receiver<(event_name, data_json)>)` pair for tests.
///
/// Unlike [`sse_channel`], the receiver yields `(String, String)` tuples
/// `(event_name, data_json_string)` so tests can inspect SSE events without
/// dealing with axum internals.
#[cfg(any(test, feature = "test-utils"))]
pub fn test_channel(buffer: usize) -> (SseSender, mpsc::Receiver<(String, String)>) {
    let (raw_tx, raw_rx) = mpsc::channel::<std::result::Result<Event, Infallible>>(buffer);
    let (named_tx, named_rx) = mpsc::channel::<(String, String)>(buffer);

    // Spawn a task that bridges the raw Event channel into (name, data) tuples.
    tokio::spawn(async move {
        let mut raw_rx = raw_rx;
        while let Some(item) = raw_rx.recv().await {
            // Result<Event, Infallible> is always Ok
            let event = item.unwrap();
            // Use the event's pretty-debug output to extract SSE wire bytes.
            // axum 0.8 Buffer debug exposes the raw SSE bytes:
            //   Event { buffer: Buffer::Active(BytesMut[b"event: stdout\ndata: ...\n"]),
            //            flags: EventFlags(3) }
            let debug = format!("{event:#?}");
            if let Some((name, data)) = parse_sse_wire(&debug) {
                let _ = named_tx.send((name, data)).await;
            }
        }
    });

    (SseSender { tx: raw_tx }, named_rx)
}

/// Parse `event:` and `data:` values from axum's pretty-debug representation
/// of an SSE `Event`.
///
/// axum 0.8 stores events as raw SSE wire bytes.  The pretty-debug output is:
/// ```text
/// Event {
///     buffer: Active(
///         b"event: stdout\ndata: {\"text\":\"hello\"}\n",
///     ),
///     ...
/// }
/// ```
/// We extract the byte-literal string, unescape it, and parse SSE lines.
#[cfg(any(test, feature = "test-utils"))]
fn parse_sse_wire(debug: &str) -> Option<(String, String)> {
    // Find the b"..." byte literal in the debug output.
    let start = debug.find("b\"")?;
    let tail = &debug[start + 2..];

    // The closing sequence in the pretty-debug is:
    //   ...last-escape-seq"\n    ),
    // The closing `"` is the first unescaped `"` followed by either `,` or `)`.
    // We scan byte-by-byte to find it.
    let bytes = tail.as_bytes();
    let mut i = 0;
    let end;
    loop {
        if i >= bytes.len() {
            return None;
        }
        if bytes[i] == b'\\' {
            // Rust byte-literal escape: skip two chars
            i += 2;
        } else if bytes[i] == b'"' {
            end = i;
            break;
        } else {
            i += 1;
        }
    }

    let raw = &tail[..end];
    let wire = unescape_rust_bytes(raw);
    parse_sse_lines(&wire)
}

#[cfg(any(test, feature = "test-utils"))]
fn unescape_rust_bytes(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    let bytes = s.as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'\\' && i + 1 < bytes.len() {
            match bytes[i + 1] {
                b'n' => { out.push('\n'); i += 2; }
                b'r' => { out.push('\r'); i += 2; }
                b't' => { out.push('\t'); i += 2; }
                b'\\' => { out.push('\\'); i += 2; }
                b'"' => { out.push('"'); i += 2; }
                b'0' => { out.push('\0'); i += 2; }
                _ => { out.push(bytes[i] as char); i += 1; }
            }
        } else {
            out.push(bytes[i] as char);
            i += 1;
        }
    }
    out
}

#[cfg(any(test, feature = "test-utils"))]
fn parse_sse_lines(wire: &str) -> Option<(String, String)> {
    let mut event_name = None::<String>;
    let mut data = None::<String>;
    for line in wire.lines() {
        if let Some(v) = line.strip_prefix("event: ") {
            event_name = Some(v.trim().to_string());
        } else if let Some(v) = line.strip_prefix("data: ") {
            data = Some(v.trim().to_string());
        }
    }
    Some((event_name?, data?))
}

pub fn sse_channel(buffer: usize) -> (SseSender, SseResponse) {
    let (tx, rx) = mpsc::channel(buffer);
    let stream = ReceiverStream::new(rx);
    let sse = Sse::new(stream).keep_alive(
        KeepAlive::new()
            .interval(Duration::from_secs(15))
            .text("ping"),
    );
    (SseSender { tx }, sse)
}

#[derive(Clone)]
pub struct SseSender {
    tx: mpsc::Sender<std::result::Result<Event, Infallible>>,
}

impl SseSender {
    pub async fn send_event(&self, event_name: &str, data: &impl serde::Serialize) -> bool {
        let json = match serde_json::to_string(data) {
            Ok(j) => j,
            Err(_) => return false,
        };
        let event = Event::default().event(event_name).data(json);
        self.tx.send(Ok(event)).await.is_ok()
    }

    pub async fn send_stdout(&self, text: &str) -> bool {
        self.send_event("stdout", &serde_json::json!({"text": text}))
            .await
    }

    pub async fn send_stderr(&self, text: &str) -> bool {
        self.send_event("stderr", &serde_json::json!({"text": text}))
            .await
    }

    pub async fn send_error(&self, text: &str, code: Option<i32>) -> bool {
        self.send_event("error", &serde_json::json!({"text": text, "code": code}))
            .await
    }

    pub async fn send_complete(&self, exit_code: i32, elapsed_ms: u64) -> bool {
        self.send_event(
            "complete",
            &serde_json::json!({"exit_code": exit_code, "elapsed_ms": elapsed_ms}),
        )
        .await
    }

    pub async fn send_result(&self, result_type: &str, content: &str) -> bool {
        self.send_event(
            "result",
            &serde_json::json!({"type": result_type, "content": content}),
        )
        .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::response::IntoResponse;
    use axum::response::sse::Sse;
    use http_body_util::BodyExt;
    use std::convert::Infallible;
    use tokio::sync::mpsc;
    use tokio_stream::wrappers::ReceiverStream;

    fn make_channel(
        buf: usize,
    ) -> (
        SseSender,
        mpsc::Receiver<std::result::Result<Event, Infallible>>,
    ) {
        let (tx, rx) = mpsc::channel(buf);
        (SseSender { tx }, rx)
    }

    /// Convert an axum `Event` to its SSE wire-format string, then extract the
    /// JSON from the `data: ` line and parse it.
    ///
    /// We wrap the single event in a one-shot `Sse` stream, call
    /// `IntoResponse::into_response()`, and drain the body bytes — this is
    /// the same path the real server takes, so the wire format is canonical.
    async fn event_to_json(event: Event) -> serde_json::Value {
        let (tx, rx) = mpsc::channel::<std::result::Result<Event, Infallible>>(1);
        tx.send(Ok(event)).await.unwrap();
        drop(tx); // close the stream so the body terminates

        let stream = ReceiverStream::new(rx);
        let sse = Sse::new(stream);
        let resp = sse.into_response();

        let body_bytes = resp
            .into_body()
            .collect()
            .await
            .expect("body collect failed")
            .to_bytes();

        let wire = std::str::from_utf8(&body_bytes).expect("utf8");
        // SSE wire format: "event: <name>\ndata: <json>\n\n"
        for line in wire.lines() {
            if let Some(json_str) = line.strip_prefix("data: ") {
                return serde_json::from_str(json_str)
                    .unwrap_or(serde_json::Value::Null);
            }
        }
        serde_json::Value::Null
    }

    /// Receive one item from the channel and return its parsed JSON data.
    async fn recv_json(
        rx: &mut mpsc::Receiver<std::result::Result<Event, Infallible>>,
    ) -> serde_json::Value {
        let event = rx.recv().await.unwrap().unwrap();
        event_to_json(event).await
    }

    // ── Basic creation ────────────────────────────────────────────────────────

    #[tokio::test]
    async fn sse_channel_creates_sender_and_response() {
        // Just verify sse_channel doesn't panic and returns usable values.
        let (sender, _response) = sse_channel(8);
        // sender should be usable
        drop(sender);
    }

    // ── send_stdout ───────────────────────────────────────────────────────────

    #[tokio::test]
    async fn send_stdout_returns_true_and_carries_text() {
        let (sender, mut rx) = make_channel(4);
        let ok = sender.send_stdout("hello world").await;
        assert!(ok);
        let val = recv_json(&mut rx).await;
        assert_eq!(val["text"], "hello world");
    }

    // ── send_stderr ───────────────────────────────────────────────────────────

    #[tokio::test]
    async fn send_stderr_returns_true_and_carries_text() {
        let (sender, mut rx) = make_channel(4);
        let ok = sender.send_stderr("error output").await;
        assert!(ok);
        let val = recv_json(&mut rx).await;
        assert_eq!(val["text"], "error output");
    }

    // ── send_complete ─────────────────────────────────────────────────────────

    #[tokio::test]
    async fn send_complete_carries_exit_code_and_elapsed() {
        let (sender, mut rx) = make_channel(4);
        let ok = sender.send_complete(0, 1234).await;
        assert!(ok);
        let val = recv_json(&mut rx).await;
        assert_eq!(val["exit_code"], 0);
        assert_eq!(val["elapsed_ms"], 1234);
    }

    // ── send_result ───────────────────────────────────────────────────────────

    #[tokio::test]
    async fn send_result_carries_type_and_content() {
        let (sender, mut rx) = make_channel(4);
        let ok = sender.send_result("text", "some content").await;
        assert!(ok);
        let val = recv_json(&mut rx).await;
        assert_eq!(val["type"], "text");
        assert_eq!(val["content"], "some content");
    }

    // ── send_error ────────────────────────────────────────────────────────────

    #[tokio::test]
    async fn send_error_with_code() {
        let (sender, mut rx) = make_channel(4);
        let ok = sender.send_error("something went wrong", Some(42)).await;
        assert!(ok);
        let val = recv_json(&mut rx).await;
        assert_eq!(val["text"], "something went wrong");
        assert_eq!(val["code"], 42);
    }

    #[tokio::test]
    async fn send_error_without_code() {
        let (sender, mut rx) = make_channel(4);
        let ok = sender.send_error("no code error", None).await;
        assert!(ok);
        let val = recv_json(&mut rx).await;
        assert_eq!(val["text"], "no code error");
        assert!(val["code"].is_null());
    }

    // ── backpressure ──────────────────────────────────────────────────────────

    #[tokio::test]
    async fn backpressure_when_receiver_dropped_send_returns_false() {
        let (sender, rx) = make_channel(1);
        drop(rx);
        let ok = sender.send_stdout("nobody is listening").await;
        assert!(!ok);
    }

    // ── clone ─────────────────────────────────────────────────────────────────

    #[tokio::test]
    async fn sender_is_clone_and_both_halves_work() {
        let (sender, mut rx) = make_channel(8);
        let sender2 = sender.clone();
        sender.send_stdout("from original").await;
        sender2.send_stdout("from clone").await;
        let v1 = recv_json(&mut rx).await;
        let v2 = recv_json(&mut rx).await;
        assert_eq!(v1["text"], "from original");
        assert_eq!(v2["text"], "from clone");
    }
}
