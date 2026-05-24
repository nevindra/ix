use serde::{Deserialize, Serialize};

// Shell
#[derive(Debug, Deserialize)]
pub struct ShellRequest {
    pub command: String,
    #[serde(default)]
    pub cwd: Option<String>,
    #[serde(default)]
    pub timeout: Option<u64>,
}

// Code execution
#[derive(Debug, Deserialize)]
pub struct CodeRequest {
    pub language: String,
    pub code: String,
    #[serde(default)]
    pub timeout: Option<u64>,
}

// File operations
#[derive(Debug, Deserialize)]
pub struct ReadFileRequest {
    pub path: String,
    #[serde(default)]
    pub offset: Option<usize>,
    #[serde(default)]
    pub limit: Option<usize>,
}

#[derive(Debug, Serialize)]
pub struct FileContent {
    pub content: String,
    pub path: String,
    pub total_lines: usize,
}

#[derive(Debug, Deserialize)]
pub struct WriteFileRequest {
    pub path: String,
    pub content: String,
}

#[derive(Debug, Deserialize)]
pub struct EditFileRequest {
    pub path: String,
    pub old: String,
    pub new: String,
}

#[derive(Debug, Deserialize)]
pub struct GlobRequest {
    pub pattern: String,
    #[serde(default = "default_dot")]
    pub path: String,
    #[serde(default)]
    pub exclude: Option<Vec<String>>,
    #[serde(default)]
    pub limit: Option<usize>,
}

#[derive(Debug, Serialize)]
pub struct GlobResult {
    pub files: Vec<String>,
    pub truncated: bool,
}

#[derive(Debug, Deserialize)]
pub struct GrepRequest {
    pub pattern: String,
    #[serde(default = "default_dot")]
    pub path: String,
    #[serde(default)]
    pub glob: Option<String>,
    #[serde(default)]
    pub context: Option<usize>,
    #[serde(default)]
    pub limit: Option<usize>,
}

#[derive(Debug, Serialize)]
pub struct GrepMatch {
    pub path: String,
    pub line: usize,
    pub content: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub context_before: Vec<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub context_after: Vec<String>,
}

#[derive(Debug, Serialize)]
pub struct GrepResult {
    pub matches: Vec<GrepMatch>,
    pub truncated: bool,
}

#[derive(Debug, Deserialize)]
pub struct TreeRequest {
    #[serde(default = "default_dot")]
    pub path: String,
    #[serde(default)]
    pub depth: Option<usize>,
    #[serde(default)]
    pub exclude: Option<Vec<String>>,
}

#[derive(Debug, Serialize)]
pub struct TreeResult {
    pub tree: String,
    pub files: usize,
    pub dirs: usize,
}

// HTTP Fetch
#[derive(Debug, Deserialize)]
pub struct HttpFetchRequest {
    pub url: String,
    #[serde(default)]
    pub raw: Option<bool>,
    #[serde(default)]
    pub max_chars: Option<usize>,
}

#[derive(Debug, Serialize)]
pub struct HttpFetchResult {
    pub url: String,
    pub title: String,
    pub content: String,
}

// Web Search
#[derive(Debug, Deserialize)]
pub struct WebSearchRequest {
    pub query: String,
    #[serde(default)]
    pub max_results: Option<usize>,
}

#[derive(Debug, Serialize)]
pub struct WebSearchResult {
    pub query: String,
    pub results: Vec<WebSearchResultItem>,
}

#[derive(Debug, Serialize)]
pub struct WebSearchResultItem {
    pub title: String,
    pub url: String,
    pub snippet: String,
}

// Browser
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BrowserAction {
    #[serde(rename = "kind")]
    pub action_type: String,
    #[serde(rename = "ref")]
    pub element_ref: Option<String>,
    pub x: Option<i32>,
    pub y: Option<i32>,
    pub text: Option<String>,
    pub key: Option<String>,
    pub direction: Option<String>,
    pub value: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BrowserResult {
    pub success: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct SnapshotOpts {
    #[serde(default)]
    pub filter: Option<String>,
    #[serde(default)]
    pub selector: Option<String>,
    #[serde(default)]
    pub depth: Option<usize>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BrowserSnapshot {
    pub url: String,
    pub title: String,
    pub nodes: Vec<SnapshotNode>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SnapshotNode {
    #[serde(rename = "ref")]
    pub element_ref: String,
    pub role: String,
    pub name: String,
}

#[derive(Debug, Clone, Deserialize, Default)]
pub struct TextOpts {
    #[serde(default)]
    pub raw: Option<bool>,
    #[serde(default)]
    pub max_chars: Option<usize>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BrowserTextResult {
    pub url: String,
    pub title: String,
    pub text: String,
    pub truncated: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BrowserFindResult {
    pub best_ref: String,
    pub confidence: String,
    pub score: f64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NavigateResult {
    pub url: String,
    pub title: String,
}

// MCP
#[derive(Debug, Deserialize)]
pub struct McpRequest {
    pub server: String,
    pub tool: String,
    #[serde(default)]
    pub args: Option<serde_json::Value>,
}

#[derive(Debug, Serialize)]
pub struct McpResult {
    pub content: String,
    pub is_error: bool,
}

// Workspace
#[derive(Debug, Serialize)]
pub struct WorkspaceInfo {
    pub os: String,
    pub arch: String,
    pub working_dir: String,
    pub tools: std::collections::HashMap<String, bool>,
    pub browser: bool,
}

// Egress
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EgressPolicy {
    pub enabled: bool,
    pub mode: PolicyMode,
    pub rules: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum PolicyMode {
    Allowlist,
    Denylist,
}

impl Default for EgressPolicy {
    fn default() -> Self {
        Self {
            enabled: false,
            mode: PolicyMode::Allowlist,
            rules: Vec::new(),
        }
    }
}

#[derive(Debug, Deserialize)]
pub struct EgressPatch {
    #[serde(default)]
    pub add: Option<Vec<String>>,
    #[serde(default)]
    pub remove: Option<Vec<String>>,
}

// SSE event types
#[derive(Debug, Serialize)]
#[serde(tag = "type", rename_all = "lowercase")]
pub enum SseEvent {
    Stdout {
        text: String,
    },
    Stderr {
        text: String,
    },
    #[serde(rename = "result")]
    ExecutionResult {
        result_type: String,
        content: String,
    },
    Error {
        text: String,
        code: Option<i32>,
    },
    Complete {
        exit_code: i32,
        elapsed_ms: u64,
    },
}

fn default_dot() -> String {
    ".".to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    // ── ShellRequest ──────────────────────────────────────────────────────────

    #[test]
    fn shell_request_full() {
        let json = r#"{"command":"ls -la","cwd":"/tmp","timeout":30}"#;
        let req: ShellRequest = serde_json::from_str(json).unwrap();
        assert_eq!(req.command, "ls -la");
        assert_eq!(req.cwd.as_deref(), Some("/tmp"));
        assert_eq!(req.timeout, Some(30));
    }

    #[test]
    fn shell_request_minimal() {
        let json = r#"{"command":"echo hi"}"#;
        let req: ShellRequest = serde_json::from_str(json).unwrap();
        assert_eq!(req.command, "echo hi");
        assert!(req.cwd.is_none());
        assert!(req.timeout.is_none());
    }

    // ── CodeRequest ───────────────────────────────────────────────────────────

    #[test]
    fn code_request_with_timeout() {
        let json = r#"{"language":"python","code":"print(1)","timeout":60}"#;
        let req: CodeRequest = serde_json::from_str(json).unwrap();
        assert_eq!(req.language, "python");
        assert_eq!(req.code, "print(1)");
        assert_eq!(req.timeout, Some(60));
    }

    #[test]
    fn code_request_without_timeout() {
        let json = r#"{"language":"rust","code":"fn main(){}"}"#;
        let req: CodeRequest = serde_json::from_str(json).unwrap();
        assert!(req.timeout.is_none());
    }

    // ── ReadFileRequest ───────────────────────────────────────────────────────

    #[test]
    fn read_file_request_defaults_to_none() {
        let json = r#"{"path":"/etc/hosts"}"#;
        let req: ReadFileRequest = serde_json::from_str(json).unwrap();
        assert_eq!(req.path, "/etc/hosts");
        assert!(req.offset.is_none());
        assert!(req.limit.is_none());
    }

    #[test]
    fn read_file_request_with_offset_and_limit() {
        let json = r#"{"path":"/etc/hosts","offset":10,"limit":50}"#;
        let req: ReadFileRequest = serde_json::from_str(json).unwrap();
        assert_eq!(req.offset, Some(10));
        assert_eq!(req.limit, Some(50));
    }

    // ── EditFileRequest ───────────────────────────────────────────────────────

    #[test]
    fn edit_file_request_round_trip() {
        let json = r#"{"path":"/a/b.rs","old":"foo","new":"bar"}"#;
        let req: EditFileRequest = serde_json::from_str(json).unwrap();
        assert_eq!(req.path, "/a/b.rs");
        assert_eq!(req.old, "foo");
        assert_eq!(req.new, "bar");
    }

    // ── GlobRequest ───────────────────────────────────────────────────────────

    #[test]
    fn glob_request_default_dot() {
        let json = r#"{"pattern":"**/*.rs"}"#;
        let req: GlobRequest = serde_json::from_str(json).unwrap();
        assert_eq!(req.path, ".");
    }

    #[test]
    fn glob_request_custom_path() {
        let json = r#"{"pattern":"*.toml","path":"/src"}"#;
        let req: GlobRequest = serde_json::from_str(json).unwrap();
        assert_eq!(req.path, "/src");
    }

    // ── EgressPolicy ─────────────────────────────────────────────────────────

    #[test]
    fn egress_policy_default_is_disabled_with_empty_allowlist() {
        let policy = EgressPolicy::default();
        assert!(!policy.enabled);
        assert!(policy.rules.is_empty());
        assert!(matches!(policy.mode, PolicyMode::Allowlist));
    }

    // ── PolicyMode ────────────────────────────────────────────────────────────

    #[test]
    fn policy_mode_allowlist_serializes_lowercase() {
        let json = serde_json::to_string(&PolicyMode::Allowlist).unwrap();
        assert_eq!(json, r#""allowlist""#);
    }

    #[test]
    fn policy_mode_denylist_serializes_lowercase() {
        let json = serde_json::to_string(&PolicyMode::Denylist).unwrap();
        assert_eq!(json, r#""denylist""#);
    }

    #[test]
    fn policy_mode_round_trips() {
        for mode in [PolicyMode::Allowlist, PolicyMode::Denylist] {
            let json = serde_json::to_string(&mode).unwrap();
            let back: PolicyMode = serde_json::from_str(&json).unwrap();
            // compare via serialized form since PolicyMode doesn't impl PartialEq
            assert_eq!(
                serde_json::to_string(&back).unwrap(),
                serde_json::to_string(&mode).unwrap()
            );
        }
    }

    // ── BrowserAction ─────────────────────────────────────────────────────────

    #[test]
    fn browser_action_all_optionals_none() {
        let json = r#"{"kind":"click","ref":null,"x":null,"y":null,"text":null,"key":null,"direction":null,"value":null}"#;
        let action: BrowserAction = serde_json::from_str(json).unwrap();
        assert_eq!(action.action_type, "click");
        assert!(action.element_ref.is_none());
        assert!(action.x.is_none());
        assert!(action.y.is_none());
        assert!(action.text.is_none());
        assert!(action.key.is_none());
        assert!(action.direction.is_none());
        assert!(action.value.is_none());
    }

    #[test]
    fn browser_action_with_coordinates() {
        let json = r#"{"kind":"move","ref":null,"x":100,"y":200,"text":null,"key":null,"direction":null,"value":null}"#;
        let action: BrowserAction = serde_json::from_str(json).unwrap();
        assert_eq!(action.x, Some(100));
        assert_eq!(action.y, Some(200));
    }

    // ── WebSearchResult ───────────────────────────────────────────────────────

    #[test]
    fn web_search_result_serializes_correctly() {
        let result = WebSearchResult {
            query: "rust async".into(),
            results: vec![WebSearchResultItem {
                title: "Tokio".into(),
                url: "https://tokio.rs".into(),
                snippet: "Async runtime".into(),
            }],
        };
        let json = serde_json::to_string(&result).unwrap();
        let val: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(val["query"], "rust async");
        assert_eq!(val["results"][0]["title"], "Tokio");
        assert_eq!(val["results"][0]["url"], "https://tokio.rs");
        assert_eq!(val["results"][0]["snippet"], "Async runtime");
    }

    // ── GrepMatch context_before / context_after skip_serializing_if ──────────

    #[test]
    fn grep_match_omits_empty_context_vecs() {
        let m = GrepMatch {
            path: "src/main.rs".into(),
            line: 42,
            content: "fn main()".into(),
            context_before: vec![],
            context_after: vec![],
        };
        let json = serde_json::to_string(&m).unwrap();
        assert!(!json.contains("context_before"));
        assert!(!json.contains("context_after"));
    }

    #[test]
    fn grep_match_includes_non_empty_context_vecs() {
        let m = GrepMatch {
            path: "src/lib.rs".into(),
            line: 10,
            content: "use foo::bar".into(),
            context_before: vec!["// comment".into()],
            context_after: vec!["let x = 1;".into()],
        };
        let json = serde_json::to_string(&m).unwrap();
        assert!(json.contains("context_before"));
        assert!(json.contains("context_after"));
    }
}
