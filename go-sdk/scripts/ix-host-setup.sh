#!/usr/bin/env bash
# ix-host-setup: one-time, root, idempotent host prep for ix preconfigured
# (rootless-manager) networking. Enables ip_forward, installs the ix-nat
# nftables table, creates a pool of owned persistent TAPs, and writes the
# manifest the manager reads (IX_PRECONFIGURED_NETWORK=1).
#
# Addressing MUST match go-sdk deriveVMNet: 172.16.0.0/16 carved into /30s,
# slot n -> host 172.16.x.(4n+1), guest .(4n+2), tap ixtap{n}. The nft rules
# below mirror go-sdk nftRuleset — keep them in sync if that Go source changes.
set -euo pipefail

TAPS=32
USERNAME=""
EGRESS_IFACE=""
GATEWAY_IP=""
CIDR="172.16.0.0/16"
MANIFEST="/etc/ix/network.json"

usage() {
  cat >&2 <<EOF
Usage: sudo ix-host-setup --taps N --user USER [--egress-iface IF] [--gateway-ip IP] [--manifest PATH]
  --taps N           number of TAP slots to pre-provision (default 32)
  --user USER        owner of the TAPs (the unprivileged service account) [required]
  --egress-iface IF  pin NAT masquerade to this uplink (default: any non-TAP iface)
  --gateway-ip IP    pin this IP on dummy ixgw0 (required for remote browser tier)
  --manifest PATH    manifest output path (default /etc/ix/network.json)
EOF
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --taps) TAPS="$2"; shift 2 ;;
    --user) USERNAME="$2"; shift 2 ;;
    --egress-iface) EGRESS_IFACE="$2"; shift 2 ;;
    --gateway-ip) GATEWAY_IP="$2"; shift 2 ;;
    --manifest) MANIFEST="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown arg: $1" >&2; usage ;;
  esac
done

[[ $EUID -eq 0 ]] || { echo "must run as root" >&2; exit 1; }
[[ -n "$USERNAME" ]] || { echo "--user is required" >&2; usage; }
id "$USERNAME" >/dev/null 2>&1 || { echo "user $USERNAME does not exist" >&2; exit 1; }
# Upper bound mirrors go-sdk maxTapIndex: index n maps to a /30 in 172.16.0.0/16,
# so n >= 16384 would spill into 172.17.x and break the manifest's CIDR.
[[ "$TAPS" =~ ^[0-9]+$ && "$TAPS" -ge 1 && "$TAPS" -le 16384 ]] || { echo "--taps must be an integer in 1..16384" >&2; exit 1; }

echo "==> enable + persist ip_forward"
sysctl -w net.ipv4.ip_forward=1 >/dev/null
echo "net.ipv4.ip_forward=1" > /etc/sysctl.d/90-ix.conf

echo "==> install ix-nat nftables table (cidr=$CIDR egress=${EGRESS_IFACE:-any})"
if [[ -n "$EGRESS_IFACE" ]]; then
  MASQ="ip saddr $CIDR oifname \"$EGRESS_IFACE\" masquerade"
else
  MASQ="ip saddr $CIDR oifname != \"ixtap*\" masquerade"
fi
nft -f - <<EOF
add table ip ix-nat
flush table ip ix-nat
add chain ip ix-nat postrouting { type nat hook postrouting priority 100 ; }
add rule ip ix-nat postrouting $MASQ
add chain ip ix-nat forward { type filter hook forward priority 0 ; }
add rule ip ix-nat forward iifname "ixtap*" accept
add rule ip ix-nat forward oifname "ixtap*" accept
EOF

# Survive a DROP forward policy (Docker): mirror ensureForwardAccept. Prefer the
# DOCKER-USER chain when present, else FORWARD. Idempotent via -C check.
if command -v iptables >/dev/null 2>&1; then
  CHAIN="FORWARD"
  iptables -S DOCKER-USER >/dev/null 2>&1 && CHAIN="DOCKER-USER"
  for DIR in -i -o; do
    iptables -C "$CHAIN" "$DIR" ixtap+ -j ACCEPT 2>/dev/null \
      || iptables -I "$CHAIN" "$DIR" ixtap+ -j ACCEPT
  done
fi

if [[ -n "$GATEWAY_IP" ]]; then
  echo "==> pin gateway IP $GATEWAY_IP on ixgw0"
  ip link show ixgw0 >/dev/null 2>&1 || ip link add ixgw0 type dummy
  ip addr show dev ixgw0 | grep -q "$GATEWAY_IP" || ip addr add "$GATEWAY_IP/32" dev ixgw0
  ip link set ixgw0 up
fi

echo "==> create $TAPS owned persistent TAPs"
TAP_JSON=""
for ((n=0; n<TAPS; n++)); do
  base=$((0xAC100000 + n*4))         # netBase + 4n
  host=$((base + 1)); guest=$((base + 2))
  ho2=$(((host>>8)&255)); ho1=$((host&255))
  go2=$(((guest>>8)&255)); go1=$((guest&255))
  hip="172.16.$ho2.$ho1"
  gip="172.16.$go2.$go1"
  mac=$(printf "06:00:AC:10:%02X:%02X" "$go2" "$go1")
  tap="ixtap$n"

  # Idempotent: recreate cleanly so owner/addr are guaranteed correct.
  ip link del "$tap" 2>/dev/null || true
  ip tuntap add "$tap" mode tap user "$USERNAME"
  ip addr add "$hip/30" dev "$tap"
  ip link set "$tap" up

  TAP_JSON+=$(printf '{"idx":%d,"name":"%s","host_ip":"%s","guest_ip":"%s","guest_mac":"%s","mask":"255.255.255.252"}' \
    "$n" "$tap" "$hip" "$gip" "$mac")
  [[ $n -lt $((TAPS-1)) ]] && TAP_JSON+=","
done

echo "==> write manifest $MANIFEST (owner=$USERNAME)"
mkdir -p "$(dirname "$MANIFEST")"
cat > "$MANIFEST" <<EOF
{
  "version": 1,
  "cidr": "$CIDR",
  "egress_iface": "$EGRESS_IFACE",
  "owner": "$USERNAME",
  "gateway_ip": "$GATEWAY_IP",
  "taps": [$TAP_JSON]
}
EOF
chmod 644 "$MANIFEST"

echo "==> done. Run the service as $USERNAME with IX_PRECONFIGURED_NETWORK=1"
