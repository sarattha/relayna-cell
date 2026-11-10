#!/usr/bin/env bash
# The only allowed host destination from Cell bridges is the egress proxy.
set -euo pipefail
engine=docker
check=0
while (($#)); do
  case $1 in
    --engine) engine=${2:?engine required}; shift 2;;
    --check) check=1; shift;;
    *) echo "Unknown network guard option: $1" >&2; exit 2;;
  esac
done
if [[ $engine == podman ]]; then
  if ((check)); then
    exec python3 /opt/relayna-cell/scripts/check-nft-guard.py
  fi
  # Replace only our table in one atomic nft transaction. Never flush host rules.
  prior=''
  if nft list table inet relayna_cell >/dev/null 2>&1; then
    python3 /opt/relayna-cell/scripts/check-nft-guard.py
    prior='delete table inet relayna_cell'
  fi
  nft -f - <<EOF
$prior
table inet relayna_cell {
  chain input {
    type filter hook input priority -10; policy accept;
    iifname "rc*" meta nfproto ipv4 tcp dport 3128 accept
    iifname "rc*" drop
  }
  chain forward {
    type filter hook forward priority -10; policy accept;
    iifname "rc*" drop
  }
}
EOF
  exec python3 /opt/relayna-cell/scripts/check-nft-guard.py
fi
[[ $engine == docker ]] || { echo 'Unknown engine' >&2; exit 2; }
if ((check)); then
  iptables -C INPUT -i 'rc+' -p tcp --dport 3128 -j ACCEPT
  iptables -C INPUT -i 'rc+' -j DROP
  iptables -C DOCKER-USER -i 'rc+' -j DROP
  ip6tables -C INPUT -i 'rc+' -j DROP
  ip6tables -C FORWARD -i 'rc+' -j DROP
  exit
fi
iptables -C INPUT -i 'rc+' -j DROP 2>/dev/null || iptables -I INPUT 1 -i 'rc+' -j DROP
# Put the narrow allow before the broad drop even when repairing partial rules.
while iptables -C INPUT -i 'rc+' -p tcp --dport 3128 -j ACCEPT 2>/dev/null; do
  iptables -D INPUT -i 'rc+' -p tcp --dport 3128 -j ACCEPT
done
iptables -I INPUT 1 -i 'rc+' -p tcp --dport 3128 -j ACCEPT
iptables -C DOCKER-USER -i 'rc+' -j DROP 2>/dev/null || iptables -I DOCKER-USER 1 -i 'rc+' -j DROP
ip6tables -C INPUT -i 'rc+' -j DROP 2>/dev/null || ip6tables -I INPUT 1 -i 'rc+' -j DROP
ip6tables -C FORWARD -i 'rc+' -j DROP 2>/dev/null || ip6tables -I FORWARD 1 -i 'rc+' -j DROP
