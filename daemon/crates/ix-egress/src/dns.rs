use crate::policy::is_allowed;
use crate::resolver::forward_query;
use hickory_proto::op::{Message, MessageType, OpCode, ResponseCode};
use hickory_proto::serialize::binary::BinDecodable;
use ix_core::error::Result;
use ix_core::types::EgressPolicy;
use std::net::SocketAddr;
use std::sync::Arc;
use tokio::net::UdpSocket;
use tokio::sync::RwLock;
use tracing::{debug, info, warn};

const MAX_DNS_PACKET: usize = 4096;
const UPSTREAM: &str = "8.8.8.8:53";

/// Write `nameserver 127.0.0.1` to `/etc/resolv.conf` so the system uses our proxy.
pub async fn write_resolv_conf() -> Result<()> {
    tokio::fs::write("/etc/resolv.conf", "nameserver 127.0.0.1\n").await?;
    Ok(())
}

/// Restore `/etc/resolv.conf` to point at a real upstream resolver on shutdown.
pub async fn restore_resolv_conf() -> Result<()> {
    tokio::fs::write("/etc/resolv.conf", "nameserver 8.8.8.8\n").await?;
    Ok(())
}

/// Run the DNS proxy until a shutdown signal is received.
///
/// Binds a UDP socket on `127.0.0.1:53` and processes each incoming DNS query:
/// - If the queried domain is allowed by the policy, the query is forwarded to
///   the upstream resolver (`8.8.8.8:53`) and the response is relayed back.
/// - If the domain is denied, an NXDOMAIN response is sent back immediately.
pub async fn run_dns_proxy(
    policy: Arc<RwLock<EgressPolicy>>,
    mut shutdown: tokio::sync::oneshot::Receiver<()>,
) -> Result<()> {
    write_resolv_conf().await?;

    let sock = Arc::new(UdpSocket::bind("127.0.0.1:53").await?);
    info!("DNS proxy listening on 127.0.0.1:53");

    let mut buf = vec![0u8; MAX_DNS_PACKET];

    loop {
        let (n, peer) = tokio::select! {
            result = sock.recv_from(&mut buf) => result?,
            _ = &mut shutdown => {
                info!("DNS proxy shutting down");
                let _ = restore_resolv_conf().await;
                return Ok(());
            }
        };

        let query_bytes = buf[..n].to_vec();
        let sock_clone = Arc::clone(&sock);
        let policy_clone = Arc::clone(&policy);

        tokio::spawn(async move {
            if let Err(e) = handle_query(query_bytes, peer, sock_clone, policy_clone).await {
                warn!("DNS query error from {peer}: {e}");
            }
        });
    }
}

async fn handle_query(
    query_bytes: Vec<u8>,
    peer: SocketAddr,
    sock: Arc<UdpSocket>,
    policy: Arc<RwLock<EgressPolicy>>,
) -> Result<()> {
    // Parse the DNS message
    let msg = match Message::from_bytes(&query_bytes) {
        Ok(m) => m,
        Err(e) => {
            warn!("Failed to parse DNS query from {peer}: {e}");
            return Ok(());
        }
    };

    // Extract the queried domain from the first question
    let domain = msg
        .queries()
        .first()
        .map(|q| q.name().to_string())
        .unwrap_or_default();

    // Strip trailing dot that hickory appends to FQDN
    let domain = domain.trim_end_matches('.').to_string();

    let current_policy = policy.read().await.clone();
    let allowed = is_allowed(&domain, &current_policy);

    if allowed {
        debug!("DNS allow: {domain}");
        match forward_query(&query_bytes, UPSTREAM).await {
            Ok(response_bytes) => {
                sock.send_to(&response_bytes, peer).await?;
            }
            Err(e) => {
                warn!("Upstream DNS error for {domain}: {e}");
                let nxdomain = build_nxdomain(&msg);
                sock.send_to(&nxdomain, peer).await?;
            }
        }
    } else {
        debug!("DNS deny: {domain}");
        let nxdomain = build_nxdomain(&msg);
        sock.send_to(&nxdomain, peer).await?;
    }

    Ok(())
}

/// Parse the queried domain name from a raw DNS query packet.
///
/// Returns `None` if the packet cannot be parsed or contains no questions.
/// Trailing dots (FQDN notation) are stripped.
#[cfg_attr(not(test), allow(dead_code))]
pub(crate) fn parse_query_domain(packet: &[u8]) -> Option<String> {
    let msg = Message::from_bytes(packet).ok()?;
    let domain = msg.queries().first()?.name().to_string();
    Some(domain.trim_end_matches('.').to_string())
}

/// Build an NXDOMAIN response for the given query message.
pub(crate) fn build_nxdomain(query: &Message) -> Vec<u8> {
    let mut response = Message::new();
    response.set_id(query.id());
    response.set_message_type(MessageType::Response);
    response.set_op_code(OpCode::Query);
    response.set_response_code(ResponseCode::NXDomain);
    response.set_recursion_desired(query.recursion_desired());
    response.set_recursion_available(true);

    // Copy the question section
    for q in query.queries() {
        response.add_query(q.clone());
    }

    response
        .to_vec()
        .unwrap_or_else(|_| build_fallback_nxdomain(query.id()))
}

/// Ultra-minimal fallback NXDOMAIN if message serialization fails.
/// 12-byte DNS header: ID | flags (QR=1, RCODE=3) | QDCOUNT=0 | ANCOUNT=0 | NSCOUNT=0 | ARCOUNT=0
pub(crate) fn build_fallback_nxdomain(id: u16) -> Vec<u8> {
    let mut pkt = vec![0u8; 12];
    pkt[0] = (id >> 8) as u8;
    pkt[1] = (id & 0xFF) as u8;
    pkt[2] = 0x81; // QR=1, Opcode=0, AA=0, TC=0, RD=1
    pkt[3] = 0x83; // RA=1, RCODE=3 (NXDOMAIN)
    pkt
}

#[cfg(test)]
mod tests {
    use super::*;
    use hickory_proto::op::Query;
    use hickory_proto::rr::{Name, RecordType};
    use std::str::FromStr;

    /// Build a minimal serialized DNS A query for the given domain name.
    fn make_query(id: u16, domain: &str) -> Vec<u8> {
        let mut msg = Message::new();
        msg.set_id(id);
        msg.set_message_type(MessageType::Query);
        msg.set_op_code(OpCode::Query);
        msg.set_recursion_desired(true);
        let name = Name::from_str(domain).expect("valid domain");
        msg.add_query(Query::query(name, RecordType::A));
        msg.to_vec().expect("serialization should succeed")
    }

    // --- parse_query_domain ---

    #[test]
    fn parse_domain_extracts_name() {
        let pkt = make_query(1234, "example.com.");
        let domain = parse_query_domain(&pkt).expect("should parse");
        assert_eq!(domain, "example.com");
    }

    #[test]
    fn parse_domain_strips_trailing_dot() {
        let pkt = make_query(42, "api.github.com.");
        let domain = parse_query_domain(&pkt).expect("should parse");
        // Trailing dot must be stripped
        assert!(!domain.ends_with('.'), "trailing dot should be stripped");
        assert_eq!(domain, "api.github.com");
    }

    #[test]
    fn parse_domain_returns_none_for_garbage() {
        assert!(parse_query_domain(&[0u8; 4]).is_none());
    }

    #[test]
    fn parse_domain_returns_none_for_empty_packet() {
        assert!(parse_query_domain(&[]).is_none());
    }

    // --- build_nxdomain ---

    #[test]
    fn nxdomain_has_correct_rcode() {
        let pkt = make_query(0xABCD, "blocked.example.com.");
        let query_msg = Message::from_bytes(&pkt).expect("valid query");
        let response = build_nxdomain(&query_msg);

        let parsed = Message::from_bytes(&response).expect("valid NXDOMAIN response");
        assert_eq!(parsed.response_code(), ResponseCode::NXDomain);
    }

    #[test]
    fn nxdomain_is_a_response() {
        let pkt = make_query(0x0001, "test.example.com.");
        let query_msg = Message::from_bytes(&pkt).expect("valid query");
        let response = build_nxdomain(&query_msg);

        let parsed = Message::from_bytes(&response).expect("valid response");
        assert_eq!(parsed.message_type(), MessageType::Response);
    }

    #[test]
    fn nxdomain_preserves_query_id() {
        let id = 0xBEEF_u16;
        let pkt = make_query(id, "blocked.org.");
        let query_msg = Message::from_bytes(&pkt).expect("valid query");
        let response = build_nxdomain(&query_msg);

        let parsed = Message::from_bytes(&response).expect("valid response");
        assert_eq!(parsed.id(), id);
    }

    #[test]
    fn nxdomain_copies_question_section() {
        let pkt = make_query(1, "test.example.com.");
        let query_msg = Message::from_bytes(&pkt).expect("valid query");
        let response = build_nxdomain(&query_msg);

        let parsed = Message::from_bytes(&response).expect("valid response");
        assert_eq!(parsed.queries().len(), 1);
        let q = &parsed.queries()[0];
        assert_eq!(q.name().to_string().trim_end_matches('.'), "test.example.com");
    }

    // --- build_fallback_nxdomain ---

    #[test]
    fn fallback_nxdomain_correct_length() {
        let pkt = build_fallback_nxdomain(0x1234);
        assert_eq!(pkt.len(), 12);
    }

    #[test]
    fn fallback_nxdomain_correct_id() {
        let pkt = build_fallback_nxdomain(0xABCD);
        assert_eq!(pkt[0], 0xAB);
        assert_eq!(pkt[1], 0xCD);
    }

    #[test]
    fn fallback_nxdomain_qr_bit_set() {
        // Byte 2: flags high byte — QR=1 means bit 7 (0x80) is set
        let pkt = build_fallback_nxdomain(0);
        assert_eq!(pkt[2] & 0x80, 0x80, "QR bit must be set (response)");
    }

    #[test]
    fn fallback_nxdomain_rcode_is_3() {
        // Byte 3 low 4 bits = RCODE; NXDOMAIN = 3
        let pkt = build_fallback_nxdomain(0);
        assert_eq!(pkt[3] & 0x0F, 3, "RCODE must be 3 (NXDOMAIN)");
    }
}
