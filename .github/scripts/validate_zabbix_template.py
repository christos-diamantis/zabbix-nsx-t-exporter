#!/usr/bin/env python3
"""Validate a Zabbix 7.0 template YAML for parseability and uniqueness.

Run in CI to catch the easy-to-miss errors that block a Zabbix import:
- malformed YAML
- duplicate UUIDs (Zabbix rejects the whole file)
- duplicate item keys within the same template
"""
from __future__ import annotations

import sys
import yaml


def collect_uuids_and_keys(doc: dict) -> tuple[list[str], list[str]]:
    uuids: list[str] = []
    keys: list[str] = []

    for g in doc["zabbix_export"].get("template_groups", []) or []:
        uuids.append(g["uuid"])
    for h in doc["zabbix_export"].get("host_groups", []) or []:
        uuids.append(h["uuid"])

    for tpl in doc["zabbix_export"].get("templates", []) or []:
        uuids.append(tpl["uuid"])

        for it in tpl.get("items", []) or []:
            uuids.append(it["uuid"])
            keys.append(it["key"])
            for tr in it.get("triggers", []) or []:
                uuids.append(tr["uuid"])

        for lld in tpl.get("discovery_rules", []) or []:
            uuids.append(lld["uuid"])
            keys.append(lld["key"])
            for proto in lld.get("item_prototypes", []) or []:
                uuids.append(proto["uuid"])
                keys.append(proto["key"])
                for tr in proto.get("trigger_prototypes", []) or []:
                    uuids.append(tr["uuid"])

    return uuids, keys


def find_duplicates(values: list[str]) -> list[str]:
    seen: set[str] = set()
    dups: list[str] = []
    for v in values:
        if v in seen and v not in dups:
            dups.append(v)
        seen.add(v)
    return dups


def main(path: str) -> int:
    with open(path, "r", encoding="utf-8") as fh:
        doc = yaml.safe_load(fh)

    uuids, keys = collect_uuids_and_keys(doc)
    dup_uuids = find_duplicates(uuids)
    dup_keys = find_duplicates(keys)

    print(f"UUIDs: {len(uuids)} total, {len(set(uuids))} unique")
    print(f"Item keys: {len(keys)} total, {len(set(keys))} unique")

    if dup_uuids:
        print(f"ERROR: duplicate UUIDs: {dup_uuids}", file=sys.stderr)
    if dup_keys:
        print(f"ERROR: duplicate item keys: {dup_keys}", file=sys.stderr)

    return 0 if not dup_uuids and not dup_keys else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1] if len(sys.argv) > 1 else "zabbix/template-nsx-t.yaml"))
