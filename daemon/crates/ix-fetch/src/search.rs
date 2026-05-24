use ix_core::{
    Result,
    types::{WebSearchRequest, WebSearchResult, WebSearchResultItem},
};
use scraper::{Html, Selector};
use tracing::debug;

const USER_AGENT: &str = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 \
    (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36";

const STARTPAGE_URL: &str = "https://www.startpage.com/sp/search";
const DEFAULT_MAX_RESULTS: usize = 10;

pub async fn web_search(req: WebSearchRequest) -> Result<WebSearchResult> {
    let query = req.query.clone();
    let max_results = req.max_results.unwrap_or(DEFAULT_MAX_RESULTS);

    debug!("Searching Startpage for: {query}, max_results={max_results}");

    let client = reqwest::Client::builder()
        .user_agent(USER_AGENT)
        .timeout(std::time::Duration::from_secs(30))
        .connect_timeout(std::time::Duration::from_secs(10))
        .redirect(reqwest::redirect::Policy::limited(10))
        .use_rustls_tls()
        .build()
        .map_err(|e| ix_core::Error::Internal(format!("failed to build HTTP client: {e}")))?;

    let response = client
        .post(STARTPAGE_URL)
        .form(&[("query", query.as_str())])
        .send()
        .await
        .map_err(|e| ix_core::Error::Internal(format!("Startpage request failed: {e}")))?;

    let body = response
        .text()
        .await
        .map_err(|e| ix_core::Error::Internal(format!("failed to read Startpage response: {e}")))?;

    let results = parse_results(&body, max_results);

    Ok(WebSearchResult { query, results })
}

#[doc(hidden)]
pub fn parse_results(html: &str, max_results: usize) -> Vec<WebSearchResultItem> {
    let document = Html::parse_document(html);
    let mut results = Vec::new();

    // Startpage result containers — try several selector patterns as the HTML structure
    // may change between deployments. We try the most specific first.
    let container_selectors = [
        ".w-gl__result",
        ".result",
        "article.w-gl__result",
        "[data-testid='result']",
    ];

    for sel_str in &container_selectors {
        if let Ok(selector) = Selector::parse(sel_str) {
            let containers: Vec<_> = document.select(&selector).collect();
            if !containers.is_empty() {
                for container in containers.into_iter().take(max_results) {
                    if let Some(item) = parse_result_container(&container) {
                        results.push(item);
                    }
                }
                if !results.is_empty() {
                    return results;
                }
            }
        }
    }

    // Fallback: try to find anchor tags with h3 headings (common result pattern)
    if results.is_empty() {
        results = parse_results_fallback(&document, max_results);
    }

    results
}

fn parse_result_container(container: &scraper::ElementRef) -> Option<WebSearchResultItem> {
    // Title: look for h2 or h3 inside the container
    let title = ["h2 a", "h3 a", "h2", "h3", ".w-gl__result-title", ".result-title"]
        .iter()
        .find_map(|sel| {
            Selector::parse(sel).ok().and_then(|s| {
                container
                    .select(&s)
                    .next()
                    .map(|el| el.text().collect::<Vec<_>>().join("").trim().to_string())
                    .filter(|t| !t.is_empty())
            })
        })?;

    // URL: look for the first anchor's href
    let url = ["h2 a", "h3 a", "a.w-gl__result-url", "a"]
        .iter()
        .find_map(|sel| {
            Selector::parse(sel).ok().and_then(|s| {
                container.select(&s).next().and_then(|el| {
                    el.value()
                        .attr("href")
                        .map(|h| h.to_string())
                        .filter(|u| u.starts_with("http"))
                })
            })
        })
        .unwrap_or_default();

    // Snippet: look for description/snippet elements
    let snippet = [
        ".w-gl__result-d",
        ".result-snippet",
        "p",
        ".description",
    ]
    .iter()
    .find_map(|sel| {
        Selector::parse(sel).ok().and_then(|s| {
            container
                .select(&s)
                .next()
                .map(|el| el.text().collect::<Vec<_>>().join("").trim().to_string())
                .filter(|t| !t.is_empty())
        })
    })
    .unwrap_or_default();

    Some(WebSearchResultItem { title, url, snippet })
}

fn parse_results_fallback(document: &Html, max_results: usize) -> Vec<WebSearchResultItem> {
    let mut results = Vec::new();

    // Look for <a href="http..."><h3>...</h3></a> patterns commonly used in search results
    let link_selector = match Selector::parse("a[href]") {
        Ok(s) => s,
        Err(_) => return results,
    };
    let h3_selector = match Selector::parse("h3") {
        Ok(s) => s,
        Err(_) => return results,
    };

    for link in document.select(&link_selector) {
        if results.len() >= max_results {
            break;
        }

        let href = match link.value().attr("href") {
            Some(h) if h.starts_with("http") => h.to_string(),
            _ => continue,
        };

        // Check if this link contains an h3 (typical search result title structure)
        let title_text = link
            .select(&h3_selector)
            .next()
            .map(|h| h.text().collect::<Vec<_>>().join("").trim().to_string());

        if let Some(title) = title_text.filter(|t| !t.is_empty()) {
            results.push(WebSearchResultItem {
                title,
                url: href,
                snippet: String::new(),
            });
        }
    }

    results
}

#[cfg(test)]
mod tests {
    use super::*;

    // ── helpers ───────────────────────────────────────────────────────────────

    /// Build a minimal Startpage-like HTML page with `n` results using the
    /// `.w-gl__result` container selector.
    fn make_startpage_html(n: usize) -> String {
        let mut body = String::from("<html><body>");
        for i in 1..=n {
            body.push_str(&format!(
                r#"<div class="w-gl__result">
                    <h3><a href="https://example.com/{i}">Result {i}</a></h3>
                    <p class="w-gl__result-d">Snippet for result {i}</p>
                </div>"#
            ));
        }
        body.push_str("</body></html>");
        body
    }

    // ── basic parsing ─────────────────────────────────────────────────────────

    #[test]
    fn parses_startpage_html_extracts_title_url_snippet() {
        let html = r#"<html><body>
            <div class="w-gl__result">
                <h3><a href="https://example.com/page">My Title</a></h3>
                <p class="w-gl__result-d">My snippet text</p>
            </div>
        </body></html>"#;
        let results = parse_results(html, 10);
        assert_eq!(results.len(), 1);
        assert_eq!(results[0].title, "My Title");
        assert_eq!(results[0].url, "https://example.com/page");
        assert!(results[0].snippet.contains("My snippet text"), "snippet: {}", results[0].snippet);
    }

    #[test]
    fn parses_multiple_results() {
        let html = make_startpage_html(5);
        let results = parse_results(&html, 10);
        assert_eq!(results.len(), 5);
        for (i, r) in results.iter().enumerate() {
            assert_eq!(r.title, format!("Result {}", i + 1));
            assert_eq!(r.url, format!("https://example.com/{}", i + 1));
        }
    }

    // ── empty / malformed input ───────────────────────────────────────────────

    #[test]
    fn empty_html_returns_empty_vec() {
        let results = parse_results("", 10);
        assert!(results.is_empty(), "expected empty, got {results:?}");
    }

    #[test]
    fn no_result_containers_returns_empty_vec() {
        let html = "<html><body><p>No results here</p></body></html>";
        let results = parse_results(html, 10);
        assert!(results.is_empty(), "expected empty, got {results:?}");
    }

    #[test]
    fn malformed_html_returns_empty_results_gracefully() {
        // scraper is lenient; ensure no panic and reasonable output
        let html = "<<<not valid html>>><div class='w-gl__result'><h3>Oops</div>";
        let results = parse_results(html, 10);
        // We only assert no panic; result count may be 0 or 1 depending on recovery
        let _ = results;
    }

    // ── max_results limiting ──────────────────────────────────────────────────

    #[test]
    fn max_results_limits_output() {
        let html = make_startpage_html(10);
        let results = parse_results(&html, 3);
        assert_eq!(results.len(), 3, "expected 3 results, got {}", results.len());
    }

    #[test]
    fn max_results_zero_returns_empty_vec() {
        let html = make_startpage_html(5);
        let results = parse_results(&html, 0);
        assert!(results.is_empty(), "expected empty for max_results=0, got {results:?}");
    }

    // ── fallback parser ───────────────────────────────────────────────────────

    #[test]
    fn fallback_parser_finds_a_h3_links() {
        // No .w-gl__result containers — should fall through to fallback
        let html = r#"<html><body>
            <a href="https://example.com/fallback"><h3>Fallback Result</h3></a>
        </body></html>"#;
        let results = parse_results(html, 10);
        assert_eq!(results.len(), 1, "fallback should find 1 result, got {results:?}");
        assert_eq!(results[0].title, "Fallback Result");
        assert_eq!(results[0].url, "https://example.com/fallback");
    }

    #[test]
    fn fallback_ignores_links_without_http_href() {
        let html = r#"<html><body>
            <a href="/relative"><h3>Relative Link</h3></a>
            <a href="https://example.com/valid"><h3>Valid</h3></a>
        </body></html>"#;
        let results = parse_results(html, 10);
        // Only the http link should be included
        assert_eq!(results.len(), 1);
        assert_eq!(results[0].url, "https://example.com/valid");
    }

    #[test]
    fn url_defaults_to_empty_string_when_no_href() {
        // Container has a title but no proper href anchor
        let html = r#"<html><body>
            <div class="w-gl__result">
                <h3>Title without href</h3>
                <p class="w-gl__result-d">Snippet</p>
            </div>
        </body></html>"#;
        let results = parse_results(html, 10);
        assert_eq!(results.len(), 1);
        assert_eq!(results[0].url, "");
    }
}
