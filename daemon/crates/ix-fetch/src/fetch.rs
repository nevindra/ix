use ix_core::{
    Result,
    types::{HttpFetchRequest, HttpFetchResult},
};
use scraper::{Html, Selector};
use std::time::Duration;
use tracing::debug;

const USER_AGENT: &str = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 \
    (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36";

const DEFAULT_MAX_CHARS: usize = 8000;

pub async fn http_fetch(req: HttpFetchRequest) -> Result<HttpFetchResult> {
    let url = req.url.clone();
    let raw = req.raw.unwrap_or(false);
    let max_chars = req.max_chars.unwrap_or(DEFAULT_MAX_CHARS);

    debug!("Fetching URL: {url}, raw={raw}, max_chars={max_chars}");

    let client = reqwest::Client::builder()
        .user_agent(USER_AGENT)
        .timeout(Duration::from_secs(30))
        .connect_timeout(Duration::from_secs(10))
        .redirect(reqwest::redirect::Policy::limited(10))
        .use_rustls_tls()
        .build()
        .map_err(|e| ix_core::Error::Internal(format!("failed to build HTTP client: {e}")))?;

    let response = client
        .get(&url)
        .send()
        .await
        .map_err(|e| ix_core::Error::Internal(format!("HTTP request failed: {e}")))?;

    let final_url = response.url().to_string();

    let body = response
        .text()
        .await
        .map_err(|e| ix_core::Error::Internal(format!("failed to read response body: {e}")))?;

    if raw {
        return Ok(HttpFetchResult {
            url: final_url,
            title: String::new(),
            content: body,
        });
    }

    let (title, content) = extract_readable(&body, max_chars);

    Ok(HttpFetchResult {
        url: final_url,
        title,
        content,
    })
}

#[doc(hidden)]
pub fn extract_readable(html: &str, max_chars: usize) -> (String, String) {
    let document = Html::parse_document(html);

    // Extract title
    let title = extract_title(&document);

    // Extract main content — try article, main, then body
    let content_selectors = ["article", "main", "body"];
    let text = content_selectors
        .iter()
        .find_map(|sel| {
            let selector = Selector::parse(sel).ok()?;
            document.select(&selector).next().map(|el| {
                el.text()
                    .collect::<Vec<_>>()
                    .join(" ")
            })
        })
        .unwrap_or_default();

    // Collapse whitespace
    let collapsed = collapse_whitespace(&text);

    // Truncate to max_chars
    let truncated = if collapsed.len() > max_chars {
        collapsed[..max_chars].to_string()
    } else {
        collapsed
    };

    (title, truncated)
}

fn extract_title(document: &Html) -> String {
    let selector = Selector::parse("title").unwrap();
    document
        .select(&selector)
        .next()
        .map(|el| collapse_whitespace(&el.text().collect::<Vec<_>>().join(" ")))
        .unwrap_or_default()
}

fn collapse_whitespace(s: &str) -> String {
    let mut result = String::with_capacity(s.len());
    let mut last_was_whitespace = false;

    for ch in s.chars() {
        if ch.is_whitespace() {
            if !last_was_whitespace && !result.is_empty() {
                result.push(' ');
            }
            last_was_whitespace = true;
        } else {
            result.push(ch);
            last_was_whitespace = false;
        }
    }

    result.trim_end().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    // ── title extraction ──────────────────────────────────────────────────────

    #[test]
    fn extracts_title_from_title_tag() {
        let html = "<html><head><title>Hello World</title></head><body></body></html>";
        let (title, _) = extract_readable(html, 10_000);
        assert_eq!(title, "Hello World");
    }

    #[test]
    fn title_is_empty_when_no_title_tag() {
        let html = "<html><head></head><body><p>No title here</p></body></html>";
        let (title, _) = extract_readable(html, 10_000);
        assert_eq!(title, "");
    }

    #[test]
    fn title_whitespace_is_collapsed() {
        let html = "<html><head><title>  Spaced   Out  </title></head><body></body></html>";
        let (title, _) = extract_readable(html, 10_000);
        assert_eq!(title, "Spaced Out");
    }

    // ── content selector priority ─────────────────────────────────────────────

    #[test]
    fn extracts_text_from_article_element() {
        let html = r#"<html><body>
            <article>Article content here</article>
            <main>Main content here</main>
        </body></html>"#;
        let (_, content) = extract_readable(html, 10_000);
        assert!(content.contains("Article content here"), "expected article content, got: {content}");
        // article is preferred; main content should not appear
        assert!(!content.contains("Main content here"), "main should be skipped when article exists");
    }

    #[test]
    fn falls_back_to_main_when_no_article() {
        let html = r#"<html><body>
            <main>Main content here</main>
        </body></html>"#;
        let (_, content) = extract_readable(html, 10_000);
        assert!(content.contains("Main content here"), "got: {content}");
    }

    #[test]
    fn falls_back_to_body_when_no_article_or_main() {
        let html = r#"<html><body><p>Body only content</p></body></html>"#;
        let (_, content) = extract_readable(html, 10_000);
        assert!(content.contains("Body only content"), "got: {content}");
    }

    // ── HTML tag stripping ────────────────────────────────────────────────────

    #[test]
    fn html_tags_are_stripped_from_content() {
        let html = r#"<html><body><article>
            <h1>Title</h1><p>Some <strong>bold</strong> text.</p>
        </article></body></html>"#;
        let (_, content) = extract_readable(html, 10_000);
        assert!(!content.contains('<'), "HTML tags should be stripped, got: {content}");
        assert!(content.contains("Title"), "got: {content}");
        assert!(content.contains("bold"), "got: {content}");
    }

    // ── whitespace collapsing ─────────────────────────────────────────────────

    #[test]
    fn whitespace_is_collapsed_in_content() {
        let html = "<html><body><article>  lots   of   spaces  \n\n  and  newlines  </article></body></html>";
        let (_, content) = extract_readable(html, 10_000);
        // No run of more than one space should exist
        assert!(!content.contains("  "), "found double-space in: {content}");
        assert!(content.contains("lots of spaces"), "got: {content}");
    }

    // ── truncation ────────────────────────────────────────────────────────────

    #[test]
    fn content_is_truncated_to_max_chars() {
        let long_text = "a".repeat(20_000);
        let html = format!("<html><body><article>{long_text}</article></body></html>");
        let (_, content) = extract_readable(&html, 500);
        assert_eq!(content.len(), 500, "content length should be exactly max_chars");
    }

    #[test]
    fn content_is_not_truncated_when_shorter_than_max_chars() {
        let html = "<html><body><article>Short text</article></body></html>";
        let (_, content) = extract_readable(html, 10_000);
        assert_eq!(content, "Short text");
    }

    #[test]
    fn zero_max_chars_returns_empty_content() {
        let html = "<html><body><article>Something</article></body></html>";
        let (_, content) = extract_readable(html, 0);
        assert_eq!(content, "");
    }

    // ── raw mode (tested via http_fetch wrapper behavior) ─────────────────────
    // Raw mode is exercised through http_fetch which requires a live HTTP call;
    // we verify the underlying extract path here instead.

    #[test]
    fn empty_html_returns_empty_title_and_content() {
        let (title, content) = extract_readable("", 10_000);
        assert_eq!(title, "");
        assert_eq!(content, "");
    }

    #[test]
    fn unicode_content_is_handled_correctly() {
        let html = "<html><head><title>日本語</title></head><body><article>こんにちは世界</article></body></html>";
        let (title, content) = extract_readable(html, 10_000);
        assert_eq!(title, "日本語");
        assert_eq!(content, "こんにちは世界");
    }
}
