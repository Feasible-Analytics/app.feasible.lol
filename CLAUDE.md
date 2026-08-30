<!--
  CLAUDE.md
  Project instructions for Claude Code working in this repository.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# feasible.lol

Privacy-first web analytics. A Go binary, per-account SQLite, Tailwind CSS, React. Three things set
it apart from the incumbents: no Docker or external database, every feature in every build, and one
flat price.

## Where the documentation lives

**There is no `docs/` directory in this repo. All planning and research lives in Harbor**, in the
**Feasible** notebook, tagged **`feasible-dev`**.

```bash
harbor search 'tag:feasible-dev' --json | jq -r '.data[]? | select(.title) | "\(.id)  \(.title)"'
harbor notes get <id> --format markdown --json | jq -r '.content'
```

| Note | What it is |
|---|---|
| **feasible.lol — Master Build Plan** | **Start here.** Architecture, data model, feature inventory, edge cases, milestones. The spec. |
| Competitive research — codebase teardown | Schemas, ingestion, tracker and query engine, read from source |
| Competitive research — feature map | Every feature in the market-leading product's documentation, with limits and formulas |
| Competitive research — dashboard UI | Layout, design tokens, API calls, filter URL encoding |
| Competitive research — dashboard screenshots | Visual reference for the dashboard |
| Competitive research — edge cases & licensing | 174 real-world edge cases, and the licensing boundaries |

Read the build plan before making architectural decisions. Most questions that look open have already
been settled there, usually with the reasoning and the rejected alternatives written down.

**Update Harbor, not a local file.** If a decision changes, edit the Harbor note so the next session
sees it.

## Licensing — read this before looking at any competitor's source

We are writing a **clean-room implementation**. Reading a competitor's code to understand an algorithm
is fine and normal. Copying it is not.

- **Never transliterate another project's code into Go.** A recognisable translation is a derivative
  work. Some of the code in this space is **AGPL**, whose network clause would force us to publish our
  source to every hosted user.
- **Some competitor code carries no license at all** — not open source, no rights granted. **Do not
  read those files as an implementation reference and do not copy from them.** Build those features
  from public documentation and first principles instead.
- **Bundled data files have their own licenses**, independent of the project that ships them. Some are
  LGPL and must stay separate, unmodified, runtime-loaded files with their license text alongside.
  Some hand-curated data files are copyrighted compilations — build our own equivalents.
- **Documentation and marketing copy is not ours to reuse.** Cover the same topics; write our own words.

**The specific repositories, licenses, file paths and boundaries are documented in Harbor** under tag
`feasible-dev`. **Read that note before opening any competitor source.**

**Wire compatibility is not a licensing problem and is a deliberate goal.** Matching an established
`/api/event` payload shape and query-API metric names is API compatibility, and it means someone can
migrate to us by changing one hostname. The exact names to match are in the Harbor compatibility
notes.

This project is **AGPL-3.0-or-later**.

## Settled decisions

Do not re-open these without a reason. Full reasoning is in the build plan.

- **Storage** — one `control.db`, plus one SQLite database per *account* (not per site).
- **Ingest** — store-and-forward. A stateless ingest tier writes derived events to a local SQLite
  outbox, answers `202`, then forwards to the shard that owns the account and deletes on ack. No
  hosted queue.
- **Routing** — shards are the source of truth. Ingestors poll each shard for its domain list. In the
  map means forward, not in the map means drop.
- **The IP address never reaches disk.** Geolocation and fingerprinting happen in the ingest tier, and
  the IP is discarded before anything is written or forwarded.
- **Front end** — React + TypeScript for the stats dashboard only; server-rendered Go templates
  everywhere else.
- **Two things must be byte-exact** or every number drifts and it is unrecoverable later: the visitor
  fingerprint and its salt rotation, and the session accumulation rules.
- **Never fail silently.** Every dropped event, truncated field and failed job must be visible to the
  customer or to us. This is the single biggest thing we are fixing about the products we compete with.

## Build and distribution

**The output is one self-contained binary.** Everything is embedded with `go:embed`: the compiled
React dashboard (JS + CSS), the server-rendered HTML templates, every tracker script variant, email
templates, SQL migrations, static assets, and the user-agent / referrer / spam-list data files.
`./feasible serve` runs with no asset directory and nothing to copy alongside it.

Two exceptions, both deliberate:

- **City-level geolocation is a separate download.** A country-level database (~6 MB, freely
  redistributable) is embedded so the binary geolocates out of the box. The city-level database is
  ~60 MB and licensed differently, so it is fetched on first run when a key is supplied. **City data is
  never a paid feature** — it is just an extra file.
- **Bot and spam lists refresh at runtime.** They go stale, so an embedded copy is only the baseline; a
  background job refreshes them into the data directory without a rebuild.

**Building requires Node; running does not.** The React dashboard and Tailwind are compiled by a
JavaScript toolchain *before* Go can embed the result, so `go build` alone is not enough from a clean
checkout — use `make build`, which runs the asset build first. Anyone downloading a release gets the
single binary with no toolchain at all.
