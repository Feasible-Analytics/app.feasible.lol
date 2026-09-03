---
name: feasible-burndown
description: >-
  Work the feasible.lol GitHub backlog end to end, one issue at a time — branch,
  build, before-and-after screenshots, a fresh-eyes review agent, then merge.
  Use when Spicer says burn down the backlog, work the issues, clear the
  backlog, or invokes /feasible-burndown. Pass "unattended" (or "going to bed",
  "walking away", "I'm out") to mean he is not around: never stop to ask, decide
  and document instead. Pairs with feasible-pm, which writes the issues.
---

# feasible-burndown — work the backlog

You take issues off the feasible.lol backlog and land them. One issue, one branch, one pull request,
merged. Then the next one. You keep going until the backlog is empty or Spicer stops you.

## Attended or unattended

Read the invocation for whether Spicer is around.

**Unattended** — he passed `unattended`, or said he is going to bed, walking away, out, or leaving it
running. Then: **never stop to ask a question.** Every fork in the road is yours to decide. Write the
decision and the reasoning into the pull request body, in a section headed `## Decisions I made`, and
carry on. A wrong decision that is documented is recoverable; a session parked on a question he will
read in the morning wasted the whole night.

**Attended** — nothing passed. You may stop and ask when a decision genuinely changes what gets
built. Keep it rare: prefer deciding and documenting, exactly as above. Ask only when getting it
wrong would mean throwing the work away.

Either way, **do not ask permission to start an issue, open a pull request, or merge one.** Invoking
this skill is that permission.

## Choosing the next issue

```bash
gh issue list --state open --limit 100 --json number,title,labels,milestone
```

Order of preference:

1. `priority:critical`, then `high`, then `medium`, then `low`, then unlabelled.
2. Within a tier, oldest first.

**Skip, and do not start:**

- Anything titled `[Needs Spicer]` — it is waiting on him by definition.
- Anything needing a credential, a third-party account, or a production action you cannot take.
- An `[Epic]` itself. Work its sub-issues; the epic closes when they do.
- Anything whose issue body says it is blocked on another open issue that is not done yet.

**Deciding not to start is free. Say which you skipped and why in your final summary.** Do not open a
pull request for an issue you never started.

## The loop, per issue

### 1. Read it properly

Read the whole issue including comments. If it names files, open them — an issue written weeks ago
may point at code that has moved. Work out what "done" means from its Definition of Done. If it has
none, write yourself one before you start.

### 2. Take the BEFORE screenshot — before you change anything

This has to happen first or it is gone. For any issue with a visible result, capture the current
behaviour now.

The app must be running, through herdr, on the Tailscale targets, in the `Server` tab panes labelled
`App`, `Ingest` and `Caddy` (see `CLAUDE.md` for the pane helper and the make targets). **You have
priority over those processes** — restart them freely, never ask.

Screenshots come from the `playwright-cli` skill. Remember that `localhost` does not work while the
`-ts` targets are running; use the MagicDNS hostname.

Skip the screenshot only when the change genuinely has no visible surface — a query-engine fix with
no UI, a refactor. Say so in the pull request rather than leaving the reader wondering.

### 3. Branch

```bash
git checkout main && git pull --ff-only
git checkout -b <short-kebab-branch>
```

One issue per branch. Never work on `main`.

### 4. Do the work

Follow `CLAUDE.md`: comment every function with **why**, never with history; one Go test file per
source file; put the header on every new file with the current date and year.

If you find an unrelated bug on the way, **fix it in this pull request** and say so in the body. Do
not open an issue for it and do not leave it.

### 5. Test

```bash
make test        # Go plus the web suite
make lint
```

Both must pass before you open the pull request. If a pre-existing failure is unrelated to your
change, say so explicitly in the body — do not quietly ship past a red suite.

### 6. Take the AFTER screenshot

Same page, same size, same theme as the before shot. A pair that does not line up proves nothing.

Upload both and embed the URLs — a repo-relative path is a broken image in a pull request:

```bash
~/.claude/skills/github-issue-image/scripts/upload-public-image.sh before.png
~/.claude/skills/github-issue-image/scripts/upload-public-image.sh after.png
```

Commit the screenshots into the repo as well when they are worth keeping, but the body links the
uploaded URLs.

### 7. Fresh-eyes review

Before you open the pull request, get it reviewed by an agent that has none of your context and none
of your investment in the approach. Spawn it with the `Agent` tool, `subagent_type: "general-purpose"`
— **never** a fork, because a fork inherits your reasoning and will agree with you.

Give it the issue and the diff and nothing else:

> You are reviewing a change with completely fresh eyes. You did not write it and you have no stake
> in it.
>
> The issue is #<N>: <paste the full issue body>.
>
> The change is on branch `<branch>`. Read it with `git diff main...<branch>`.
>
> Answer four things, concretely, quoting file and line:
> 1. Does this actually satisfy every line of the issue's Definition of Done? List anything missed.
> 2. Is it correct? Name the inputs or state that would break it.
> 3. Does it match the surrounding code — comment style, naming, test layout, the rules in CLAUDE.md?
> 4. What would a careful reviewer object to?
>
> Do not praise it. If it is fine, say so in one line and stop.

**Act on what comes back.** Fix what is real. If you disagree with a finding, say why in the pull
request body rather than silently ignoring it. If it finds something big, fix it and review again —
but no more than twice; a third round means the issue was underspecified, so write that down and
ship what you have.

### 8. Pull request

```bash
gh pr create --title "<what it does>" --body-file <scratchpad>/pr.md
```

The body carries:

- **What changed**, in a few lines.
- **Before / after**, the two uploaded images side by side. Skip with a reason if there is no visual.
- **Decisions I made** — every fork you took alone, and why. This is the section Spicer actually
  reads. Do not skip it because the decisions felt obvious.
- **Fresh-eyes review** — what it found and what you did about it.
- **Anything unrelated you fixed on the way.**
- `Closes #<N>`.

No "Generated with Claude Code", no co-author line — in the pull request or in any commit.

**Print the full pull request URL** to Spicer as soon as it exists, and again on every push to it.

### 9. Merge

Once the screenshots are in and the fresh-eyes review is answered, merging is yours to do. Do not ask.

```bash
gh pr merge <N> --squash --delete-branch
git checkout main && git pull --ff-only
git branch -d <branch>          # separate step; --delete-branch does not do the local one
```

The end state is always: on `main`, up to date, feature branch gone from `git branch`.

### 10. Next issue

Go back to the top. Keep going.

## When you get blocked

**Before starting** — skip it. Say which and why in the summary. Nothing else.

**Halfway through** — do not throw the work away and do not sit on it:

1. Commit what you have, working or not.
2. Push the branch and open the pull request, titled `[Blocked] <what it was going to do>`.
3. In the body: what you did, exactly what stopped you, what you tried, and what you need from
   Spicer to finish. Be specific — "needs a decision" is useless, "needs to know whether hiding
   zero-conversion goals should also hide them from the CSV export" is actionable.
4. **Do not merge it.**
5. Move to the next issue.

A blocked pull request is a good outcome. A session that stopped at the first hard question is not.

## Reporting back

While you work, keep it short: the issue number, one line on what you did, the pull request URL.

At the end, one summary:

- Merged: issue numbers and titles.
- Blocked: issue numbers, the pull request URL, and the one question each needs answered.
- Skipped: issue numbers and the reason.
- Anything you found that is not yet an issue — describe it; do not create the issue. That is what
  `feasible-pm` is for.
