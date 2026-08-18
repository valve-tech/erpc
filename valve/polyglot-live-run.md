# Three chain families, one binary — the live run

Date: 2026-08-17. Binary: `go build -o /tmp/erpc ./cmd/erpc` at commit
`6880a01`. Config: [polyglot-live-pool.yaml](polyglot-live-pool.yaml).

This run tests the fork's central claim: **adding a chain is a configuration
exercise, not new Go code.** One eRPC process served Ethereum mainnet, Solana
mainnet-beta and Bitcoin mainnet at the same time, against live public
endpoints, from one config file.

## Verdict

**The claim holds for the run, with one exception.**

I wrote no Go code. `git diff` for this task touches two files, and both are
documents plus one YAML config. The three families all served correct data from
the same process.

The exception is real and I reproduced it: `common.IsValidNetwork`
(`common/network.go:95`) still enumerates `evm:` and `svm:` by hand, so a
`btc:mainnet` network cannot be named in a provider's `onlyNetworks` or
`ignoreNetworks` list. That path needs Go. Nothing else did. See entry 91 in
[upstream-bug-log.md](upstream-bug-log.md).

The honest caveat on the headline: the fork already ships `architecture/btc` as
a compiled Go package. So the claim this run tests is "**serving** a third
family is configuration", not "**inventing** one is". Inventing a fourth family
still needs two Go edits — a file implementing `common.ChainFamily`, and one
blank import in `erpc/chain_families.go`, because Go links packages at compile
time. That is a small, bounded cost, and it needs no new constant, no new
switch case and no edit to any pipeline file.

---

## 1. The endpoints

I ran a `curl` at every candidate before it entered the config. An endpoint that
answered `401`, `403`, `429` or `521` did not go in.

| Family | Endpoint | Result |
|---|---|---|
| EVM | `https://ethereum-rpc.publicnode.com` | in — 200, `0x1` |
| EVM | `https://eth.drpc.org` | in — 200, `0x1` |
| EVM | `https://docs-demo.quiknode.pro` | in — 200, `0x1` |
| EVM | `https://cloudflare-eth.com` | in, fallback tier — serves the tip, `-32603` on an old block |
| EVM | `https://1rpc.io/eth` | in, fallback tier — same limit |
| EVM | `https://eth.llamarpc.com` | rejected — HTTP 521 |
| EVM | `https://eth.merkle.io` | rejected — HTTP 429 |
| EVM | `https://ethereum.blockpi.network/v1/rpc/public` | rejected — HTTP 521 |
| SVM | `https://api.mainnet-beta.solana.com` | in — 200, `ok` |
| SVM | `https://solana-rpc.publicnode.com` | in — 200, `ok` |
| SVM | `https://docs-demo.solana-mainnet.quiknode.pro` | in — 200, `ok` |
| SVM | `https://solana.drpc.org` | rejected — "chain is not available on free plan" |
| SVM | `https://rpc.ankr.com/solana`, `mainnet.rpcpool.com` | rejected — 403 |
| SVM | `api.blockeden.xyz`, `solana.gateway.tatum.io`, `public-sol.nownodes.io` | rejected — paid plan or 404 |
| BTC | the six from [btc-live-pool.yaml](btc-live-pool.yaml) | all still answer |

Keyless Solana RPC is now scarce. Three endpoints was all I could find, and one
of them (publicnode) sits behind Cloudflare and resets unauthenticated
connections now and then.

**No API keys appear in the config, and none is needed.** Every endpoint above
is public and keyless.

## 2. What one process served

eRPC bootstrapped all three networks in 404 ms:

```
"networkId":"evm:1"             "upstreams ready for network initialization"
"networkId":"svm:mainnet-beta"  "upstreams ready for network initialization"
"networkId":"btc:mainnet"       "upstreams ready for network initialization"
"message":"networks bootstrap completed"  state: ready
```

Nine client requests, three per family, all HTTP 200.

### EVM — `POST /main/evm/1`

| Request | Answer | Served by |
|---|---|---|
| `eth_chainId` | `0x1` | eRPC itself, no upstream |
| `eth_blockNumber` | `0x1895bec` (25,779,180) | `evm-drpc` |
| `eth_getBlockByNumber ["0xC5D488", false]` | hash `0x9b83c12c…eee71` | `evm-drpc` |

### SVM — `POST /main/svm/mainnet-beta`

| Request | Answer | Served by |
|---|---|---|
| `getHealth` | `"ok"` | `svm-publicnode` |
| `getSlot` | `439976427` | `svm-publicnode` |
| `getGenesisHash` | `5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d` | eRPC itself, no upstream |

### BTC — `POST /main/btc/mainnet`

| Request | Answer | Served by |
|---|---|---|
| `getblockchaininfo` | `chain: main`, `blocks: 962976`, `initialblockdownload: false` | `btc-drpc` |
| `getblockhash [800000]` | `00000000000000000002a7c4c1e48d76c5a37902165a270156b7a8d72728a054` | `btc-drpc` |
| `getblock [<that hash>, 1]` | `height: 800000`, `nTx: 3721`, `time: 1690168629` | `btc-drpc` |

## 3. Correctness, not just HTTP 200

A balancer that answers 200 with wrong data is worse than one that errors. So
each family got an assertion against a value that is fixed and published.

| Family | Assertion | Expected | eRPC returned | Verdict |
|---|---|---|---|---|
| EVM | chain id | `0x1` | `0x1` | pass |
| EVM | hash of block 12,965,000 (the London fork block) | `0x9b83c12c69edb74f6c8dd5d052765c1adf940e320bd1291696e6fa07829eee71` | identical | pass |
| SVM | mainnet-beta genesis hash | `5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d` | identical | pass |
| SVM | `getBlock` at finalized slot 439,977,161 | blockhash `Fjdd19Bz72obH7kGPjD3NDNJeBeM6XtyTydHYsjJXKDv`, height 418,027,552 | identical | pass |
| BTC | hash of block 800,000 | `00000000000000000002a7c4c1e48d76c5a37902165a270156b7a8d72728a054` | identical | pass |
| BTC | `getblock` on that hash reports its own height | `800000` | `800000` | pass |

I sourced the two block hashes twice, from two independent providers, before I
compared them to eRPC's answer. The Solana block came from an eRPC upstream and
from a direct call to a different provider; the two agree exactly.

Two of these assertions do **not** exercise an upstream, and I say so plainly.
`eth_chainId` and `getGenesisHash` are short-circuited by architecture hooks
(`architecture/evm/hooks.go:38`, `architecture/svm/hooks.go:143`) and never
leave the process. They prove eRPC's own bookkeeping is right, not that an
endpoint answered. The load-bearing assertions are the three block reads, and
all three went to a real upstream.

## 4. Failover

Killing a public endpoint is not an option, so I put eRPC in front of two local
pass-through proxies, `evm-shim-a` (forwards to drpc) and `evm-shim-b`
(forwards to publicnode), and killed the one eRPC actually chose. The SVM and
BTC pools stayed on their real public endpoints throughout.

**Phase 1 — both shims alive.** 8 `eth_blockNumber` requests, 8 successes, all
served by `evm-shim-b`:

```
erpc_network_successful_request_total{attempt="1",category="eth_blockNumber",
  network="evm:1",upstream="evm-shim-b"} 8
```

**Phase 2 — I killed `evm-shim-b`, the chosen upstream.** 8 more requests:

```
  1 chainId=0x1  blockNumber=0x1895c05
  ...
  8 chainId=0x1  blockNumber=0x1895c06
  eth_blockNumber successes: 8 / 8
```

```
erpc_network_successful_request_total{attempt="2",category="eth_blockNumber",
  network="evm:1",upstream="evm-shim-a"} 8
erpc_upstream_request_errors_total{category="eth_blockNumber",
  error="ErrEndpointTransportFailure",severity="critical",
  upstream="evm-shim-b"} 8
erpc_upstream_attempt_outcome_total{outcome="transport_error",
  upstream="evm-shim-b"} 8
```

Every request hit the dead upstream first, took a transport error, and finished
on `evm-shim-a` inside the same client request. `attempt` went from 1 to 2. No
client saw a failure: `erpc_network_failed_request_total` and
`erpc_network_no_upstreams_available_total` have no samples for `evm:1` at all.

The other two families kept serving while the EVM pool was degraded. Bitcoin
returned the block-800000 hash again, and Solana answered `getHealth` with
`ok`.

## 5. The metrics, read carefully

I scraped `/metrics` and diffed it around a single request, because the
`erpc_upstream_*` counters do not mean what their names suggest.

**Client traffic lives in `erpc_network_*`.** Across both phases the counters
are clean and complete:

```
erpc_network_request_received_total{category="eth_blockNumber",network="evm:1"}    16
erpc_network_request_received_total{category="eth_chainId",network="evm:1"}        16
erpc_network_request_received_total{category="getBlock",network="svm:mainnet-beta"} 1
erpc_network_request_received_total{category="getHealth",network="svm:mainnet-beta"} 1
erpc_network_request_received_total{category="getSlot",network="svm:mainnet-beta"}  1
erpc_network_request_received_total{category="getblockhash",network="btc:mainnet"}  1
```

Received totals equal successful totals for every family. Zero client-visible
failures.

**Three traps caught me, and I record them as bug-log entries 92, 93 and 94.**

1. A single `getblockhash` produced two upstream calls, and the second one was
   a *prober* mirror against an excluded upstream, not routing. It still
   increments `erpc_upstream_request_total` with `composite="none"` and
   `erpc_upstream_selection_total{reason="primary"}`, carrying the client's
   `agent_name="curl"`. Only the duration histogram marks it `composite="probe"`.
   So `tier:fallback` was working correctly all along, and the counters made it
   look like the fallback tier was taking primary traffic.
2. Eight in-request failovers produced **zero** samples on
   `erpc_network_retry_attempt_total`. Reading that counter as "how often did we
   fail over" reports zero during a real outage.
3. `upstream="n/a"` is not only a cache hit. My config has no cache at all, and
   three methods still landed there — the two hook short-circuits above, plus
   `getSlot` with `commitment: finalized`.

## 6. What needed Go code

Nothing, for this run. The precise inventory:

| Layer | Did a third family force an edit? |
|---|---|
| `architecture/btc/family.go` | Already present. It defines its own architecture name locally, so it adds no constant anywhere else. |
| `erpc/chain_families.go` | Already present — one blank import. Go links at compile time, so this line is unavoidable for any new family. |
| `common.NetworkArchitecture` / `UpstreamType` | No constant. `type: btc` resolves through the registry. |
| `IsValidArchitecture`, `IsValidNetworkId`, `Network.prepareRequest`, the client factory | All registry lookups. No switch case. |
| `Upstream.detectFeatures` | Has a registry-driven `else` branch (`detectChainFamilyFeatures`). A btc upstream bootstraps and forwards. |
| Health tracker, selection policy, failsafe, breaker | Chain-agnostic already. Untouched. |
| **`common.IsValidNetwork`** | **Yes.** Hard-codes `evm:` and `svm:`. A `btc:` network in `providers[].onlyNetworks` fails config load. |

That last row is the whole gap, and it is narrow: it only blocks the provider
mechanism, not plain `upstreams:`. My config uses `upstreams:` and never met it.

One fork comment is now wrong, and I record it as entry 90.
`erpc/chain_families.go` still says "a btc UPSTREAM does not bootstrap … no btc
request is ever forwarded". This run forwarded 5 btc requests to real Bitcoin
nodes and got correct blocks back. The second half of that comment, about
`IsValidNetwork`, is still accurate.

## 7. Reproducing this

```sh
export GOPROXY=https://proxy.golang.org,direct
go build -o /tmp/erpc ./cmd/erpc
/tmp/erpc valve/polyglot-live-pool.yaml

curl -s -XPOST -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0xC5D488",false]}' \
  http://127.0.0.1:4211/main/evm/1

curl -s -XPOST -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"getGenesisHash","params":[]}' \
  http://127.0.0.1:4211/main/svm/mainnet-beta

curl -s -XPOST -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"getblockhash","params":[800000]}' \
  http://127.0.0.1:4211/main/btc/mainnet
```

For the failover phase, add a killable upstream to the EVM pool and stop it
mid-run:

```yaml
      - id: evm-shim-b
        type: evm
        endpoint: http://127.0.0.1:4298
        evm:
          chainId: 1
```

Any pass-through JSON-RPC proxy on that port works. Send it a browser-like
`User-Agent`; publicnode answers Python's default urllib agent with HTTP 403.

Public endpoints rate-limit. Keep the request volume low. This whole run used
about 40 client requests.
