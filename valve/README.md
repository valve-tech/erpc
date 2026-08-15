# valve/ — fork-owned documents

This directory belongs to the `valve-tech/erpc` fork. Upstream `erpc/erpc` has
no directory of this name, so nothing here can conflict when the fork merges
from upstream. Do not put fork documents in `docs/` — that is upstream's
Next.js documentation site, and it changes on most merges.

| Document | What it is |
|---|---|
| [upstream-bug-log.md](upstream-bug-log.md) | **Start here.** 35 bugs in upstream-inherited code, 1 in the fork's own code, plus the fixes the fork already carries that upstream still needs. Every entry cites `file:line` and was verified in source. |
| [polyglot-feasibility.md](polyglot-feasibility.md) | Can the fork serve non-EVM chains from configuration rather than new Go code? Verdict and evidence. |
| [fork-reconcile.md](fork-reconcile.md) | The commit triage that brought the fork up to current upstream. |
| [merge-review.md](merge-review.md) | Review of that merge. Carries a correction: the first pass recorded the `erpc` package as green when the run had been truncated by Go's default 600s timeout. |
| [fallback-escape-decision.md](fallback-escape-decision.md) | Why the per-request fallback escape belongs in the selection policy, not in Go. |

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
