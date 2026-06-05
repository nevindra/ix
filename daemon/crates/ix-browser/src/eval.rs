//! Shared wire format for pinchtab's `/evaluate` response.
//!
//! pinchtab returns `{"result": <value>}` where `<value>` is the RAW JSON
//! value of the evaluated expression (`evaluate.go:HandleEvaluate` writes
//! `map[string]any{"result": result}`): `1+1` yields `{"result": 2}`, a DOM
//! query may yield an object, `document.title` a string. The old daemon-side
//! struct declared `result: String`, so every non-string expression failed
//! with "error decoding response body" — eval only ever worked for
//! string-valued expressions. Both backends (in-VM and remote gateway) parse
//! through this type so the fix cannot diverge.

/// Deserialized `/evaluate` response. `result` defaults to `Null` so a
/// missing field (expression evaluated to `undefined`) is not a parse error.
#[derive(serde::Deserialize)]
pub(crate) struct EvalResponse {
    #[serde(default)]
    pub result: serde_json::Value,
}

impl EvalResponse {
    /// Convert the result to the string form the `BrowserBackend::eval`
    /// contract promises: strings pass through unquoted, everything else is
    /// rendered as JSON text (`2`, `true`, `null`, `{"a":1}`).
    pub fn into_string(self) -> String {
        match self.result {
            serde_json::Value::String(s) => s,
            v => v.to_string(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(body: &str) -> EvalResponse {
        serde_json::from_str(body).expect("EvalResponse must parse")
    }

    #[test]
    fn string_result_passes_through_unquoted() {
        assert_eq!(parse(r#"{"result":"hi"}"#).into_string(), "hi");
    }

    #[test]
    fn number_result_renders_as_json_text() {
        // The exact shape pinchtab returns for the benchmark's `1+1`.
        assert_eq!(parse(r#"{"result":2}"#).into_string(), "2");
    }

    #[test]
    fn bool_result_renders_as_json_text() {
        assert_eq!(parse(r#"{"result":true}"#).into_string(), "true");
    }

    #[test]
    fn object_result_renders_as_json_text() {
        assert_eq!(parse(r#"{"result":{"a":1}}"#).into_string(), r#"{"a":1}"#);
    }

    #[test]
    fn array_result_renders_as_json_text() {
        assert_eq!(parse(r#"{"result":[1,2]}"#).into_string(), "[1,2]");
    }

    #[test]
    fn null_result_renders_as_null() {
        assert_eq!(parse(r#"{"result":null}"#).into_string(), "null");
    }

    #[test]
    fn missing_result_field_defaults_to_null() {
        // Expressions evaluating to `undefined` may omit the field entirely.
        assert_eq!(parse("{}").into_string(), "null");
    }
}
