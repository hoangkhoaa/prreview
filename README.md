# prreview

AI-assisted GitHub PR review & approve in your terminal, with a bugbot-grade
multi-pass review pipeline and a PM2-style watch daemon. A Go/Bubble Tea port
of the `github-pr-review-approve` agent skill (`~/.warp/skills`).

## Review pipeline (bugbot architecture)

```
fetch PR ──► full file context ──► 4 parallel finders ──► dedupe ──► adversarial ──► line ──► summary
(gh CLI)     (head revision of      correctness            + line     verifiers       snap     + verdict
              changed files)        security               snapping   (kill false
                                    resilience                        positives)
                                    hygiene
```

- **Context beyond the diff** — full head-revision content of changed files is
  fetched (lock files/binaries/vendor skipped) so the model sees surrounding
  code, not just hunks.
- **Coverage-first finders** — specialized parallel "lenses" are told to report
  everything, including uncertain findings, with confidence scores.
- **Adversarial verification** — a second stage actively tries to refute each
  candidate against the code; only findings a strong human reviewer would
  stand behind survive. This is what keeps false positives low.
- **Line snapping** — findings are validated against parsed diff hunks; lines
  are snapped to the nearest commentable line or demoted to the review body,
  so GitHub never rejects the review.
- **Prompt caching** (Anthropic) — the large shared context is cached; verify
  and summary calls read the cache written by the finder calls.

### Depth setting (intelligence vs cost)

| depth | finders | verify | effort | context |
|---|---|---|---|---|
| `fast` | 1 combined | no | medium | 10 files / 80 KB |
| `standard` | 2 lenses | yes | high | 20 files / 120 KB |
| `max` (default) | 4 lenses | yes | xhigh | 30 files / 200 KB |

## Install

Requires Go 1.22+ and the [gh CLI](https://cli.github.com) (`gh auth login`).

```sh
make build            # → bin/prreview
make install          # → /usr/local/bin/prreview
make release          # darwin arm64 + amd64 binaries
```

## Configure

`prreview config` opens a Cursor-style settings form (↑↓ field, ←→ choose):

```
prreview settings

› Provider   [anthropic]  openai
  Base URL   https://llm.internal.example.com
  API key    ************
  Model      deepseek-chat
  Depth       fast   standard  [max]
```

- **Anthropic**: leave Base URL empty, model e.g. `claude-opus-4-8`.
- **Self-hosted / OpenAI-compatible** (vLLM, Ollama, LiteLLM, DeepSeek...):
  provider `openai`, Base URL of your endpoint (with or without `/v1`), model
  as served by your endpoint.

Non-interactive:

```sh
prreview config set provider openai
prreview config set base_url https://llm.internal.example.com
prreview config set api_key sk-...
prreview config set model deepseek-chat
prreview config set depth max
```

Config: `~/.config/prreview/config.json` (0600). Env overrides:
`PRREVIEW_API_KEY` / `ANTHROPIC_API_KEY`, `PRREVIEW_BASE_URL`.

## Review a PR

```sh
prreview https://github.com/org/repo/pull/42        # review and approve
prreview review  https://github.com/org/repo/pull/42
prreview approve https://github.com/org/repo/pull/42
```

Keys: `a` approve · `r` request changes · `c` comment · `↑/↓` scroll ·
`q` quit without posting.

Safety rules (from the skill): own PR → comment only; approving with findings
requires y/n confirmation; approval bodies never empty; PR closed/merged →
stop.

## Model profiles (Cursor-style)

Define named models once, switch anytime:

```sh
prreview model                 # interactive: enter=set default, a add, e edit, d delete
prreview model list
prreview model use deepseek    # set default model
prreview model rm old-one
```

```
prreview models

› ● deepseek       openai     deepseek-chat      https://ds.internal/v1
    claude         anthropic  claude-opus-4-8    (default endpoint)
```

Each profile has its own provider / endpoint / model id, and optionally its
own API key (empty = reuse the global `api_key`). Override per run with
`-m/--model`:

```sh
prreview https://github.com/org/repo/pull/42 -m claude
prreview watch --daemon -m deepseek
```

## Web UI

```sh
prreview serve                 # http://127.0.0.1:8787 — opens the browser
prreview serve --port 9000 --no-open -m deepseek

prreview serve --detach        # run the web UI in the background
prreview serve status|logs|stop
```

The UI is styled as a reviewer's desk: paper background, monospace data,
red-pencil annotations for findings, and rubber-stamp verdicts
(APPROVED / CHANGES REQUESTED) on posted reviews.

**First run?** Just `prreview serve` — no prior setup needed. If nothing is
configured yet, the browser shows an onboarding screen: pick provider
(Anthropic / OpenAI-compatible), enter endpoint + API key + model ID, choose
depth, and you land straight in the dashboard. The same form is reachable
later via the **＋ model** button to add more profiles. **Test connection**
verifies key + endpoint + model with a tiny completion before you save; the
**test** button next to the model dropdown re-checks the active profile
anytime (CLI: `prreview model test [name]`).

A friendly local dashboard:

- **Paste a PR URL → Review** — the job runs through the full pipeline; when
  done you see the summary + findings and click **Approve / Request changes /
  Comment** (own PRs get comment-only; approving with findings asks to
  confirm).
- **Watch toggle** — polling for review requests right from the header; those
  jobs auto-post like the daemon (`auto-approve clean` opt-in). By default only
  PRs where you are **directly** requested are picked up (not team requests) —
  the **filters** button adds team requests and a **repo whitelist** (only
  those repos are auto-queued; manually pasted URLs always review directly).
- **Live job list** — status filter tabs + pagination, running stage,
  cancel / re-review / remove per job.
- **Model & depth switchers** in the header — same profiles as `prreview model`.
  **Load models** in the add-model form lists everything the endpoint serves
  (`/v1/models`) so you can pick, or tick several to add them all as profiles
  under one key (CLI: `prreview model discover [name]`).
- **purge** clears queued + finished jobs (CLI: `prreview purge`); **⏻ off**
  is the kill switch — cancels every review, stops watch (including the watch
  daemon process) and shuts the engine down until you `prreview serve` again.

The server binds `127.0.0.1` only (it posts reviews with your gh identity —
don't expose it). UI is a single embedded page; no external assets.

## Watch daemon (PM2-style)

```sh
prreview watch                        # dashboard: poll every 15s
prreview watch --interval 30 --auto-approve --concurrency 3

prreview watch --daemon               # headless: run in the background
prreview watch status                 # is it running?
prreview watch logs -f                # follow logs (~/.config/prreview/watch.log)
prreview watch stop                   # SIGTERM, finishes in-flight reviews
```

Polls `review-requested:@me`, auto-reviews new PRs, and posts a COMMENT review
with line-level findings (`--auto-approve` additionally approves clean PRs —
it never auto-approves a PR with findings).

```
prreview watch   polling every 15s · model deepseek-chat · depth max

  STATUS      PR                          DETAIL                          TIME
  ⠋ running   cake/pay-core#812           verifying 6 candidate findings…  41s
  ✓ done      cake/pay-core#809           COMMENT · 3 findings             2m 10s
  ○ queued    cake/gateway#233            queued

  ↑↓ select · x cancel · d remove · r re-review · o open · q quit
```

- `x` cancels a running review mid-flight, `d` removes a row (never picked up
  again), `r` forces a re-review, `o` opens the PR in the browser.
- State persists at `~/.config/prreview/watch-state.json`, so restarts don't
  re-review PRs already handled (a new head commit is not auto-re-reviewed;
  use `r`).

## Notes

- Reviews go through the GitHub REST API (`gh api`) — no commits are created,
  so GPG signing is not involved in approving a PR.
- Diffs are capped at ~180 KB; full-file context is capped per depth profile.
