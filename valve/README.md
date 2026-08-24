# valve/ — fork-owned documents

This directory belongs to the `valve-tech/erpc` fork. Upstream `erpc/erpc` has
no directory of this name, so nothing here can conflict when the fork merges
from upstream. Do not put fork documents in `docs/` — that is upstream's
Next.js documentation site, and it changes on most merges.

| Document | What it is |
|---|---|
| [upstream-bug-log.md](upstream-bug-log.md) | **Start here.** 178 entries: 107 fixed in the fork, 53 open, 18 judged not a bug. Every entry cites `file:line` and was verified in source. Read its "Reading this file" section before you grep it — ids are sparse and out of order, and a section header is weaker evidence than an entry's own Status line. |
| [open-entry-triage.md](open-entry-triage.md) | What to do with the log's open entries, now that the fork rebases instead of sending pull requests. Ten of them obey one rule at ten different sites, and two of those are now fixed; fourteen were dead code and are now closed. |
| [polyglot-feasibility.md](polyglot-feasibility.md) | Can the fork serve non-EVM chains from configuration rather than new Go code? Verdict and evidence. |
| [fork-reconcile.md](fork-reconcile.md) | The commit triage that brought the fork up to current upstream. |
| [merge-review.md](merge-review.md) | Review of that merge. Carries a correction: the first pass recorded the `erpc` package as green when the run had been truncated by Go's default 600s timeout. |
| [fallback-escape-decision.md](fallback-escape-decision.md) | Why the per-request fallback escape belongs in the selection policy, not in Go. |
| [billing-module.md](billing-module.md) | The design of `valvebilling/` — the valve billing path as a flag-gated Go package with a zero-file diff against upstream. Why the metering decision stays in Lua inside Redis and is never reimplemented in Go, and what can still break quietly. |
| [periodic-enforcement.md](periodic-enforcement.md) | Serving billing from state eRPC refreshes on a timer, instead of from a blocking call. Quantifies what a cache TTL costs in overdraft, and finds that eRPC already ships the enforcement machinery — the blocker is that its key strategy looks up by the raw credential. |
| [polyglot-live-run.md](polyglot-live-run.md) | One binary serving EVM, SVM and BTC at once against live public endpoints. Tests the "a chain is configuration, not Go" claim end to end. Config: [polyglot-live-pool.yaml](polyglot-live-pool.yaml). |

## Checking the log

Two scripts. Both take the log's path and default to it.

`check-bug-log.sh` runs on every commit that touches the log. It refuses a
duplicate entry id, an entry whose status line count is not exactly one, a
status spelled outside the key's four words, and an entry filed under a section
header that claims a different id range. A merge that keeps both sides of a
hunk is what it catches: two status paragraphs under one heading, disagreeing.

`check-test-citations.sh` is a report you run by hand, and the right time is
**after a rebase** — that is when upstream renames a test and a citation rots in
silence. It lists every test the log names that the tree does not have, with the
line each name sits on:

```sh
valve/check-test-citations.sh
```

It is deliberately NOT a commit hook. Its first run, on 2026-08-21, reported
six names, and all six were correct prose — each sat in a sentence saying the
test was deleted or replaced. A hook that fails six times on the day it lands
gets `--no-verify`, and the bypass takes the conflict-marker check with it.
Read the lines it prints, and add a hook only if a citation is ever found
genuinely rotted.

Expect the count to grow. Renaming a test correctly ADDS a name here, because
the entry then says "X is replaced by Y" and names both. A rising count is the
document working, not rotting.

## Running the tests

Two facts that cost real time to learn.

**Never run `go test ./erpc/`.** That package holds 457 tests and 927 `gock`
mocks sharing global HTTP-transport state, so it cannot run in parallel in one
process. Serially it needs about 970 seconds, and Go's default timeout is 600 —
so the run is cut off and reports nothing about the tests it never reached. **A
truncated run looks exactly like a pass**, and that has already hidden a real
regression here.

Use `make test-fast`, which compiles the test binary once and shards it across
processes so each shard gets its own gock state. It covers the whole repo in
about 10 minutes. CI already uses it.

**The container-backed tests need the right Docker socket.** Two Docker installs
are common on macOS. If `docker info` succeeds but tests still report "Is the
docker daemon running?", testcontainers has found a stale Docker Desktop socket:

```sh
export GOPROXY=https://proxy.golang.org,direct   # or builds stall ~20 min at 0% CPU
export DOCKER_HOST=unix://$HOME/.colima/default/docker.sock
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
make test-fast
```
