#!/usr/bin/env python3
"""Validate the actual Podman guard structure, including order and hook priority."""
import json
import subprocess

data = json.loads(subprocess.check_output(["nft", "-j", "list", "table", "inet", "relayna_cell"]))
chains = {}
rules = {"input": [], "forward": []}
for item in data["nftables"]:
    if "chain" in item:
        c = item["chain"]
        chains[c["name"]] = c
    if "rule" in item:
        r = item["rule"]
        rules.setdefault(r["chain"], []).append(r["expr"])

def match(left, right):
    return {"match": {"op": "==", "left": left, "right": right}}

interface = match({"meta": {"key": "iifname"}}, "rc*")
expected = {
    "input": [[interface, match({"meta": {"key": "nfproto"}}, "ipv4"),
               match({"payload": {"protocol": "tcp", "field": "dport"}}, 3128), {"accept": None}],
              [interface, {"drop": None}]],
    "forward": [[interface, {"drop": None}]],
}
assert set(chains) == set(expected), "unexpected guard chains"
for name, chain in chains.items():
    assert chain["hook"] == name and chain["type"] == "filter" and chain["prio"] == -10
    assert chain["policy"] == "accept"
assert rules == expected, "guard rules changed or are out of order"
