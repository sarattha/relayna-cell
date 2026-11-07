#!/usr/bin/env bash
# The only allowed host destination from Cell bridges is the egress proxy.
set -euo pipefail
iptables -C INPUT -i 'rc+' -j DROP 2>/dev/null || iptables -I INPUT 1 -i 'rc+' -j DROP
iptables -C INPUT -i 'rc+' -p tcp --dport 3128 -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -i 'rc+' -p tcp --dport 3128 -j ACCEPT
iptables -C DOCKER-USER -i 'rc+' -j DROP 2>/dev/null || iptables -I DOCKER-USER 1 -i 'rc+' -j DROP
ip6tables -C INPUT -i 'rc+' -j DROP 2>/dev/null || ip6tables -I INPUT 1 -i 'rc+' -j DROP
ip6tables -C FORWARD -i 'rc+' -j DROP 2>/dev/null || ip6tables -I FORWARD 1 -i 'rc+' -j DROP
