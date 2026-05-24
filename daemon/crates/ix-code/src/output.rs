/// Map a MIME type string to a result_type label used in SSE events
pub fn map_mime_to_result_type(mime: &str) -> &str {
    match mime {
        "text/plain" => "text",
        "text/html" => "html",
        "image/png" => "png",
        "image/jpeg" => "jpeg",
        "image/svg+xml" => "svg",
        "application/pdf" => "pdf",
        "text/latex" => "latex",
        "application/json" => "json",
        "text/markdown" => "markdown",
        _ => "text",
    }
}

/// Priority order for MIME type selection (richest first)
const MIME_PRIORITY: &[&str] = &[
    "image/png",
    "image/jpeg",
    "image/svg+xml",
    "text/html",
    "text/markdown",
    "text/latex",
    "application/json",
    "text/plain",
];

/// Extract the best (richest) output from a Jupyter `data` dict.
/// Returns `(result_type, content)`.
pub fn extract_best_output(data: &serde_json::Value) -> (String, String) {
    let obj = match data.as_object() {
        Some(o) => o,
        None => return ("text".into(), String::new()),
    };

    for mime in MIME_PRIORITY {
        if let Some(val) = obj.get(*mime) {
            let content = match val {
                serde_json::Value::String(s) => s.clone(),
                other => other.to_string(),
            };
            return (map_mime_to_result_type(mime).to_string(), content);
        }
    }

    // Fall back to the first available key
    if let Some((mime, val)) = obj.iter().next() {
        let content = match val {
            serde_json::Value::String(s) => s.clone(),
            other => other.to_string(),
        };
        return (map_mime_to_result_type(mime).to_string(), content);
    }

    ("text".into(), String::new())
}

#[cfg(test)]
mod tests {
    use super::*;

    // -----------------------------------------------------------------------
    // map_mime_to_result_type
    // -----------------------------------------------------------------------

    #[test]
    fn mime_text_plain_maps_to_text() {
        assert_eq!(map_mime_to_result_type("text/plain"), "text");
    }

    #[test]
    fn mime_image_png_maps_to_png() {
        assert_eq!(map_mime_to_result_type("image/png"), "png");
    }

    #[test]
    fn mime_text_html_maps_to_html() {
        assert_eq!(map_mime_to_result_type("text/html"), "html");
    }

    #[test]
    fn mime_image_svg_xml_maps_to_svg() {
        assert_eq!(map_mime_to_result_type("image/svg+xml"), "svg");
    }

    #[test]
    fn mime_image_jpeg_maps_to_jpeg() {
        assert_eq!(map_mime_to_result_type("image/jpeg"), "jpeg");
    }

    #[test]
    fn mime_application_pdf_maps_to_pdf() {
        assert_eq!(map_mime_to_result_type("application/pdf"), "pdf");
    }

    #[test]
    fn mime_text_latex_maps_to_latex() {
        assert_eq!(map_mime_to_result_type("text/latex"), "latex");
    }

    #[test]
    fn mime_application_json_maps_to_json() {
        assert_eq!(map_mime_to_result_type("application/json"), "json");
    }

    #[test]
    fn mime_text_markdown_maps_to_markdown() {
        assert_eq!(map_mime_to_result_type("text/markdown"), "markdown");
    }

    #[test]
    fn mime_unknown_falls_back_to_text() {
        assert_eq!(map_mime_to_result_type("application/octet-stream"), "text");
    }

    #[test]
    fn mime_empty_string_falls_back_to_text() {
        assert_eq!(map_mime_to_result_type(""), "text");
    }

    // -----------------------------------------------------------------------
    // extract_best_output
    // -----------------------------------------------------------------------

    #[test]
    fn extract_prefers_png_over_text() {
        let data = serde_json::json!({
            "text/plain": "some text",
            "image/png": "base64encodedpng=="
        });
        let (result_type, content) = extract_best_output(&data);
        assert_eq!(result_type, "png");
        assert_eq!(content, "base64encodedpng==");
    }

    #[test]
    fn extract_prefers_html_over_text() {
        let data = serde_json::json!({
            "text/plain": "plain",
            "text/html": "<b>rich</b>"
        });
        let (result_type, content) = extract_best_output(&data);
        assert_eq!(result_type, "html");
        assert_eq!(content, "<b>rich</b>");
    }

    #[test]
    fn extract_returns_text_when_only_text_available() {
        let data = serde_json::json!({
            "text/plain": "hello world"
        });
        let (result_type, content) = extract_best_output(&data);
        assert_eq!(result_type, "text");
        assert_eq!(content, "hello world");
    }

    #[test]
    fn extract_empty_data_returns_empty_text() {
        let data = serde_json::json!({});
        let (result_type, content) = extract_best_output(&data);
        assert_eq!(result_type, "text");
        assert_eq!(content, "");
    }

    #[test]
    fn extract_non_object_returns_empty_text() {
        let data = serde_json::json!(null);
        let (result_type, content) = extract_best_output(&data);
        assert_eq!(result_type, "text");
        assert_eq!(content, "");
    }

    #[test]
    fn extract_priority_png_over_jpeg() {
        let data = serde_json::json!({
            "image/jpeg": "jpeg-data",
            "image/png": "png-data"
        });
        let (result_type, _) = extract_best_output(&data);
        assert_eq!(result_type, "png");
    }

    #[test]
    fn extract_priority_jpeg_over_svg() {
        let data = serde_json::json!({
            "image/svg+xml": "<svg/>",
            "image/jpeg": "jpeg-data"
        });
        let (result_type, _) = extract_best_output(&data);
        assert_eq!(result_type, "jpeg");
    }

    #[test]
    fn extract_priority_svg_over_html() {
        let data = serde_json::json!({
            "text/html": "<b>html</b>",
            "image/svg+xml": "<svg/>"
        });
        let (result_type, _) = extract_best_output(&data);
        assert_eq!(result_type, "svg");
    }

    #[test]
    fn extract_priority_html_over_markdown() {
        let data = serde_json::json!({
            "text/markdown": "# title",
            "text/html": "<h1>title</h1>"
        });
        let (result_type, _) = extract_best_output(&data);
        assert_eq!(result_type, "html");
    }

    #[test]
    fn extract_priority_markdown_over_latex() {
        let data = serde_json::json!({
            "text/latex": "$E=mc^2$",
            "text/markdown": "**bold**"
        });
        let (result_type, _) = extract_best_output(&data);
        assert_eq!(result_type, "markdown");
    }

    #[test]
    fn extract_priority_latex_over_json() {
        let data = serde_json::json!({
            "application/json": {"key": "val"},
            "text/latex": "$x$"
        });
        let (result_type, _) = extract_best_output(&data);
        assert_eq!(result_type, "latex");
    }

    #[test]
    fn extract_priority_json_over_plain_text() {
        let data = serde_json::json!({
            "text/plain": "plain",
            "application/json": {"x": 1}
        });
        let (result_type, _) = extract_best_output(&data);
        assert_eq!(result_type, "json");
    }

    #[test]
    fn extract_priority_full_chain_png_wins() {
        let data = serde_json::json!({
            "text/plain": "plain",
            "text/html": "<b>html</b>",
            "image/svg+xml": "<svg/>",
            "image/jpeg": "jpeg",
            "image/png": "png"
        });
        let (result_type, content) = extract_best_output(&data);
        assert_eq!(result_type, "png");
        assert_eq!(content, "png");
    }

    #[test]
    fn extract_non_string_value_converts_to_string() {
        // application/json value is a JSON object, not a string — should be serialized
        let data = serde_json::json!({
            "application/json": {"key": "value"}
        });
        let (result_type, content) = extract_best_output(&data);
        assert_eq!(result_type, "json");
        // content should be non-empty (the JSON serialization of the object)
        assert!(!content.is_empty());
    }
}
