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

## Issue conventions

- **Epics are titled `[Epic] <name>`.** Never `EPIC:` or any other prefix.
- Epics own **native GitHub sub-issues**, not markdown checklists — only native sub-issues give a real
  parent/child rollup and populate the project board's Parent and Progress fields.
- Child issue bodies end with `Part of #<epic>`.
- Every issue carries an `area:*` label, a `priority:*` label, and a milestone.
- **Issues must stand alone.** A senior developer with no prior context should be able to pick one up
  and do the work from the issue body. The Harbor plan is background, not a prerequisite — this repo
  is public and Harbor is not.
- **Never name a competitor in this repository**, in issues, commits, code or comments. Say "the
  incumbent" or "a competitor". Specifics live in Harbor.
- **Images in an issue or PR body must be public URLs, never repo-relative paths.** GitHub fetches
  every image through its camo proxy with no authentication, so a path like
  `internal/dashboard/screenshots/01-light.png` renders in a committed file but is a **broken image**
  in a PR description. Upload it first and embed the URL it prints:

  ```bash
  ~/.claude/skills/github-issue-image/scripts/upload-public-image.sh path/to/shot.png
  # -> https://screen-capture-osx.s3.us-east-1.amazonaws.com/public/<32-hex>.png
  ```

  Commit the screenshot to the repo as well when it is worth keeping — but the PR body links the
  uploaded URL.

## Running the local apps — always through herdr

**The agent has priority over the local processes at all times.** If you see `feasible`, Caddy or any
of our dev processes running, you may stop, restart or kill them without asking. You never need
permission to take a port or restart a server.

**But always do it through herdr, never with a bare `go run` from your own shell.** Both of us drive
the same three panes, so we see the same logs and neither of us is left wondering why a port is busy.

The three processes live in the **`Server` tab**, one per labelled pane:

| Pane label | Runs | Port |
|---|---|---|
| **App** | `make app-ts` | 19301 (internal 19401, loopback only) |
| **Ingest** | `make ingest-ts` | 19302 |
| **Caddy** | `make caddy-ts` | 19300 |

### Always start the Tailscale variant

**Default to the `-ts` targets, not the plain ones.** Spicer is often on a different machine and
reaches the running app over Tailscale. A server bound to loopback is invisible to him, so binding to
loopback wastes his time and yours.

- Use `make app-ts` / `make ingest-ts` / `make caddy-ts` unless there is a specific reason not to.
- **Fall back to the plain targets only when Tailscale is not running.** Say so when you do, so he
  knows why he cannot reach it.
- **Always print the URL after starting**, using the MagicDNS name rather than the IP —
  `http://<machine>.<tailnet>.ts.net:19300`. He needs something he can click.

**Two consequences to remember:**

1. **`localhost` will not work** while the `-ts` targets are running, because the listeners are bound
   to the Tailscale address rather than loopback. **Your own `curl` checks must use the Tailscale
   hostname too.** That is deliberate: binding `0.0.0.0` would also expose the app to whatever café
   or hotel network the laptop is on.
2. **`BASE_URL` must be the MagicDNS hostname**, not the IP, and the `-ts` targets set it alongside
   the bind address. Get this wrong and cookies will not set, redirects bounce, and Google OAuth
   rejects the redirect URI — all with no useful error message.

The **internal listener stays on `127.0.0.1` even in `-ts` mode.** Putting `/internal/*` on the
tailnet would expose the salts endpoint — the one that can reverse visitor fingerprints — to every
device on it.

**Find a pane by label, never by a hard-coded id** — herdr compacts ids when panes close:

```bash
herdr_pane() {
  herdr pane list | python3 -c "
import sys, json
label = '$1'
panes = json.load(sys.stdin)['result']['panes']
ws = next((p['workspace_id'] for p in panes if p.get('focused')), None)
print(next((p['pane_id'] for p in panes
            if p.get('workspace_id') == ws and p.get('label') == label), ''))
"
}
```

**Start, stop, and read:**

```bash
herdr pane run "$(herdr_pane App)" "make app-ts"       # start (Tailscale — the default)
herdr pane send-keys "$(herdr_pane App)" C-c           # stop
herdr pane read "$(herdr_pane App)" --source recent --lines 50   # read the logs

# wait for it to actually be up before hitting it
herdr wait output "$(herdr_pane App)" --match "listening" --timeout 30000
```

**If the `Server` tab or any of its three panes is missing, create it** — do not fall back to running
things in your own shell:

```bash
WS=$(herdr pane list | python3 -c "import sys,json;print(next(p['workspace_id'] for p in json.load(sys.stdin)['result']['panes'] if p.get('focused')))")
TAB=$(herdr tab create --workspace "$WS" --label "Server" --no-focus \
      | python3 -c "import sys,json;print(json.load(sys.stdin)['result']['tab']['tab_id'])")
# then split the root pane right, and the right pane down, and label the three panes
# App / Ingest / Caddy to match the table above.
```

**Rules:**

- **Never start a long-running server with the Bash tool.** It blocks, the output is invisible to
  Spicer, and it leaves an orphan process he cannot stop.
- **Always read the pane after starting something** rather than assuming it came up.
- Stopping is `C-c` to the pane, not `pkill` — that keeps the shell alive and the pane reusable.
- `make dev-solo` runs everything in one process; use the **App** pane for it and leave the other two
  idle. Its Tailscale twin is `make dev-solo-ts`.
- **After starting anything, tell Spicer the Tailscale URL.** He may well be on another machine, and a
  running server he cannot find is the same as no server at all.

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
