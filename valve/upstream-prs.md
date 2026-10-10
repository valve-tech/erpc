# Upstream PRs

Every erpc/erpc pull request the fork carries, proposes, or waits on. One row
per PR. Update a row when its state changes, and when an upstream merge makes
a fork commit redundant.

- **Ours:** we opened it. Branches live on `valvecitydev/erpc`, one fix per PR.
- **Pulled:** someone else's PR that the fork took before upstream released it.
- **Waiting:** someone else's open PR that fixes a bug we logged. Not in the fork.

Fork commit is the commit on `main` that carries the same change.

| PR | Kind | What | Bug log | Fork commit | State |
|---|---|---|---|---|---|
| [#1217](https://github.com/erpc/erpc/pull/1217) | Ours | consensus: preferNonEmpty wins a tie for the lead | 192 (issue #1215) | `8e7af61f` | open |
| [#1218](https://github.com/erpc/erpc/pull/1218) | Ours | consensus: count answers already received when the wait cap fires | 193 (issue #1216) | `34aa6ff2` | open |
| [#1219](https://github.com/erpc/erpc/pull/1219) | Ours | test(gzip): wait for the cache entry before asserting a HIT | — | `2c7ef225` | open |
| [#1220](https://github.com/erpc/erpc/pull/1220) | Ours | test(hedge): only the server timeout ends the server-timeout case | — | `616952c3` | open |
| [#1221](https://github.com/erpc/erpc/pull/1221) | Ours | test(consensus): accept 2 or 3 calls where slot order decides | — | `45a3f2ff` | open |
| [#1222](https://github.com/erpc/erpc/pull/1222) | Ours | test(consensus): misbehavior scoring waits for every participant | — | `1aa407ae` | open |
| [#1223](https://github.com/erpc/erpc/pull/1223) | Ours | test(hedge): only the hedge answers in SimpleHedgePolicy | — | `01bc4a5b` | open |
| [#1224](https://github.com/erpc/erpc/pull/1224) | Ours | test(evm): wait for the cached sub-range before split getLogs | — | `320c08ec` | open |
| [#1226](https://github.com/erpc/erpc/pull/1226) | Ours | policy: eval timeout wins, without losing the first ordering | 46, 194 (issue #1225) | `8ad1b4cf` + `bc5a2246` | open |
| [#1232](https://github.com/erpc/erpc/pull/1232) | Ours | test(hedge): check which delay the hedge used, not runner speed | — | `ab8aa74c` | open |
| [#1197](https://github.com/erpc/erpc/pull/1197) | Waiting | evm: caller gas caps and rejected tracers are not server faults | 191 (issue #1175) | fork's own fix in 191 | open |
| [#1184](https://github.com/erpc/erpc/pull/1184) | Pulled | docker: pin pnpm so release images build | — | upstream merge `39392b73` | merged upstream |
| [#1180](https://github.com/erpc/erpc/pull/1180) | Pulled | consensus: hash missing-data by verdict | — | upstream merge `39392b73` | merged upstream |
| [#1156](https://github.com/erpc/erpc/pull/1156) | Pulled | cacheKeySuffix isolates JSON-RPC cache partitions | — | upstream merge `39392b73` | merged upstream |
| [#1179](https://github.com/erpc/erpc/pull/1179) | Pulled | grpc: opt-in client health checking for BDS pools | — | upstream merge `39392b73` | merged upstream |

To refresh the states:

```sh
for n in 1217 1218 1219 1220 1221 1222 1223 1224 1226 1232 1197; do
  gh pr view $n -R erpc/erpc --json number,state -q '"#\(.number) \(.state)"'
done
```
