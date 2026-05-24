use ix_core::error::Result;
use std::time::Duration;
use tokio::net::UdpSocket;
use tokio::time::timeout;

const UPSTREAM_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_DNS_PACKET: usize = 4096;

/// Forward a raw DNS query to an upstream resolver and return the raw response.
///
/// Opens an ephemeral UDP socket, sends `query_bytes` to `upstream` (e.g. `"8.8.8.8:53"`),
/// waits up to 5 seconds for a reply, and returns the reply bytes.
pub async fn forward_query(query_bytes: &[u8], upstream: &str) -> Result<Vec<u8>> {
    // Bind to an ephemeral local port
    let sock = UdpSocket::bind("0.0.0.0:0").await?;
    sock.connect(upstream).await?;
    sock.send(query_bytes).await?;

    let mut buf = vec![0u8; MAX_DNS_PACKET];
    let n = timeout(UPSTREAM_TIMEOUT, sock.recv(&mut buf))
        .await
        .map_err(|_| ix_core::error::Error::Timeout("upstream DNS timeout".into()))??;

    buf.truncate(n);
    Ok(buf)
}
