#!/usr/bin/env python3
"""Check .codebase-map/map.json and render it to .codebase-map/page.html.

Run from the repo root:  python3 .claude/skills/codebase-map/build.py
Exits 1 and lists every problem if the map breaks a rule. Makes no network calls.
"""
import html
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
MAP = ".codebase-map/map.json"
OUT = ".codebase-map/page.html"
KINDS = {"entry", "cron", "agent", "model", "tool", "service", "store", "external"}
NODE_CAPS = {"label": 28, "sub": 40, "detail": 200, "sourceRef": 120}


def check(d):
    bad = []
    nodes, edges = d["graph"]["nodes"], d["graph"]["edges"]
    ids = [n["id"] for n in nodes]
    if len(set(ids)) != len(ids):
        bad.append("node ids are not unique")
    known = set(ids)
    for n in nodes:
        if n.get("kind") not in KINDS:
            bad.append(f"{n['id']}: unknown kind {n.get('kind')!r}")
        for key, cap in NODE_CAPS.items():
            if len(n.get(key, "")) > cap:
                bad.append(f"{n['id']}: {key} is over {cap} characters")
        ref = n.get("sourceRef")
        if ref and not os.path.exists(ref.split(":")[0]):
            bad.append(f"{n['id']}: sourceRef {ref} does not exist")
    pairs = set()
    for e in edges:
        if e["from"] not in known or e["to"] not in known:
            bad.append(f"edge {e['from']} -> {e['to']} names an unknown node")
        if len(e.get("label", "")) > 24:
            bad.append(f"edge {e['from']} -> {e['to']}: label is over 24 characters")
        pairs.add((e["from"], e["to"]))
    for f in d.get("flows", []):
        for a, b in f["edges"]:
            if (a, b) not in pairs:
                bad.append(f"flow {f['id']}: no edge {a} -> {b}")
    placed = [i for col in d["layout"]["columns"] for i in col]
    for i in placed:
        if i not in known:
            bad.append(f"layout names unknown node {i}")
    if len(set(placed)) != len(placed):
        bad.append("layout places a node twice")
    for i in known - set(placed):
        bad.append(f"layout does not place {i}")
    return bad


def main():
    with open(MAP) as f:
        d = json.load(f)
    bad = check(d)
    if bad:
        print("\n".join(bad))
        sys.exit(1)
    with open(os.path.join(HERE, "template.html")) as f:
        page = f.read()
    # "</" inside JSON would end the <script> block early.
    data = json.dumps(d, ensure_ascii=False).replace("</", "<\\/")
    page = page.replace("__TITLE__", html.escape(d["project"]["name"] + " Codebase Map"))
    page = page.replace("__MAP_JSON__", data)
    with open(OUT, "w") as f:
        f.write(page)
    g = d["graph"]
    print(f"{OUT}: {len(g['nodes'])} nodes, {len(g['edges'])} edges, {len(d.get('flows', []))} flows")


if __name__ == "__main__":
    main()
