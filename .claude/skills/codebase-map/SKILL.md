---
name: codebase-map
description: Analyze this repository and publish an interactive map of how the codebase works and how it uses AI, as a private page on claude.ai. Use when the user asks for a codebase map or scan, or to update one. It uploads nothing to foglamp.dev or any other third-party service.
---

Analyze THIS repository and draw an interactive map of how it works and how it
uses AI. You write only the data (a JSON file). The template in this skill's
folder draws the page, so write no HTML or CSS for the map.

## Privacy rule
Never send the map to foglamp.dev or any other third-party service. The only
place it may be published is a private Artifact on claude.ai. Do not make it
public or share it unless the user asks. Keep it high level: no code, secrets,
hostnames, IP addresses or known weaknesses.

## Steps
1. Investigate the repo (see below) and write `.codebase-map/map.json`. If it
   already exists, update it rather than starting over.
2. Add `.codebase-map/` to `$(git rev-parse --git-common-dir)/info/exclude` if
   it isn't there, so the files stay out of commits without changing .gitignore.
3. From the repo root, run `python3 .claude/skills/codebase-map/build.py`. It
   checks the map and writes `.codebase-map/page.html`, without using the
   network. Fix anything it reports and run it again.
4. Publish `.codebase-map/page.html` with the Artifact tool. The template is
   already designed, so skip the design steps.
   - First run: pass a one-sentence `description` and `icon: "map"`, then save
     the returned URL to `.codebase-map/artifact-url.txt`.
   - Later runs: if `artifact-url.txt` exists, read that artifact with
     `action: "read"`, then publish to the same `url` so the link stays the same.
5. Give the user the link.

## How to investigate
- Check the branch you're on and map that. If important work exists only on
  another branch, say so in `project.note`.
- Find where AI runs: SDK calls (generateText, streamText, messages.create, and
  so on), agent loops, tool definitions, MCP servers. Identify the models,
  providers and the tools models can call. If there is no AI, say so and use 0.
- Map the business logic: internal services and pipelines (billing, ingestion,
  background workers), with the interesting sentence on the edge.
- Map the main flows: entry points (routes, webhooks, pages, CLIs), scheduled
  jobs, agents, the models and tools they use, and what they read and write.
- Verify claims against the code. Memory and docs can be out of date.

## Output: write exactly this shape to .codebase-map/map.json
{
  "version": 1,
  "project": {
    "name": "string",
    "tagline": "one line (optional)",
    "note": "anything a reader would otherwise get wrong (optional)",
    "date": "YYYY-MM-DD"
  },
  "stats": { "agents": 0, "models": 0, "tools": 0, "integrations": 0 },
  "topIntegrations": [ { "id": "stripe", "label": "Stripe" } ],
  "graph": {
    "nodes": [
      { "id": "agent", "label": "Support agent", "kind": "agent", "sub": "streamText",
        "sourceRef": "src/agents/support.ts:42",
        "detail": "Answers tickets with order lookups" }
    ],
    "edges": [
      { "from": "billing", "to": "pg", "kind": "writes", "label": "charges on trial end" }
    ]
  },
  "flows": [
    { "id": "signup", "label": "Sign-up", "summary": "One or two sentences.",
      "edges": [ ["chat", "agent"], ["agent", "gpt4o"] ] }
  ],
  "layout": { "columns": [ ["chat", "cron"], ["agent"], ["gpt4o", "pg"] ] }
}

## Rules (build.py enforces most of these)
- Aim for 20-40 nodes on a substantial codebase, at most 60 nodes and 120 edges.
  Every node must earn its place.
- Give each distinct agent its own node when there are 10 or fewer. Merge only
  numerous, near-identical agents, and say so in `sub`.
- Node `kind`: entry (trigger, route, page, CLI), cron (scheduled job), agent,
  model, tool, service (internal module or pipeline), store (DB, cache, index,
  queue), external (third-party API).
- Edge `kind`: calls, reads, writes or triggers. Add a `label` only when a
  specific phrase says more.
- Lengths: node label 28, sub 40, detail 200, sourceRef 120, edge label 24.
  Add `sourceRef` to internal nodes, and check that the path exists.
- `flows`: 3-6 named paths a reader would ask about, listed in order. The first
  one is highlighted when the page opens, so make it the product's main path.
  Each pair must be an existing edge.
- `layout.columns`: place every node exactly once, left to right from what sends
  data to where it rests, in 6-8 columns of at most 7 nodes. Order each column to
  keep related nodes level and reduce crossings.
- Use today's date for `project.date`.
