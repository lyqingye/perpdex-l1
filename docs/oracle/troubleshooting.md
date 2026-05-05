# Oracle troubleshooting

A short list of failure modes we've actually hit in development, in the
order in which they tend to surface.

---

## 1. `OraclePrice` store stays empty after starting the chain

**Symptoms.** `perpd query oracle prices` returns no rows; risk /
liquidation reject orders with `oracle price not found`.

**Diagnosis.**

```sh
# Is the daemon enabled?
grep -A5 '\[oracle\]' ~/.perpd/config/app.toml
# Does the sidecar reachable from the chain process?
grpcurl -plaintext localhost:8080 perpdex.oracle.sidecar.v1.Oracle/Prices
# Are vote extensions enabled at consensus level?
perpd query consensus params abci
```

**Fix.**

- If the sidecar reachability check fails: see step 2.
- If `[oracle].enabled` is missing or `false`: set it to `true` and
  restart the chain. The daemon defaults to disabled to be safe on
  non-validator nodes.
- If `vote_extensions_enable_height` is `0`: that means cometbft has
  never been told to enable VEs. You can either bump it via consensus
  governance, or restart the chain from genesis with the auto-default
  applied by `app.InitChainer` (`DefaultVoteExtensionsEnableHeight = 2`).

---

## 2. Sidecar starts but prices never populate

**Symptoms.** `grpcurl Oracle.Prices` returns an empty `prices` map even
after several seconds.

**Diagnosis.** Look at the sidecar logs:

```sh
journalctl -fu oracle-sidecar
# or
docker logs -f oracle-sidecar
```

You'll typically see one of:

- `[binance] fetch failed: dial tcp: i/o timeout` — outbound DNS or HTTPS
  is being filtered. Open up `api.binance.com:443` for the host.
- `[coingecko] http 429: Rate limit exceeded` — bump
  `providers.coingecko.interval` or supply an `api_key`.
- `[okx] decode: ... invalid character ...` — the upstream returned an
  HTML error page. Almost always a regional block; switch to a different
  egress IP or disable `okx`.

If at least one provider is publishing, prices should appear within
`max_age` seconds. If not, `min_sources` may be too high relative to the
number of healthy providers.

---

## 3. ProcessProposal rejects every block

**Symptoms.** `perpd start` log line `oracle preblock: insufficient
committed voting power`, blocks not produced.

**Cause.** Fewer than 2/3+ of the committed voting power broadcast a
vote-extension. With <4 validators on a dev chain, even a single
validator missing a VE drops the chain below quorum.

**Fix.** On a dev chain just make sure every validator has the daemon
enabled and pointed at a working sidecar. On testnet/mainnet this is the
canary that should page operators: a real consensus-level oracle
outage requires explicit validator coordination.

---

## 4. Vote-extension size exceeds block limit

**Symptoms.** `cometbft` rejects proposals with
`block size > MaxBytes`.

**Cause.** Lots of markets × lots of validators makes the
ExtendedCommitInfo blob big. With 100 validators × 32 markets the raw
encoding is ~120 KB; with 100 markets it's pushing 400 KB.

**Fix.** The chain already wraps the EC bytes in a zstd frame
(`ZstdExtendedCommitCodec`) which typically yields a 4-6× compression.
If even that is not enough you have two options:

1. Reduce per-validator markets in vote extensions: configure your
   sidecar with a smaller `pairs` list per validator (validators don't
   all need to vote on every market).
2. Bump cometbft's `consensus.max_block_size_bytes` via governance.

---

## 5. Daemon dial errors but the sidecar is up

**Symptoms.** Chain logs `oracle daemon poll failed: rpc error:
connection refused`.

**Cause.** The sidecar binds `:8080` on a non-default address (e.g.
`127.0.0.1:8080` only) but the chain dials a different one.

**Fix.** Make `oracle.sidecar_address` in `app.toml` match
`grpc_addr` in `oracle.json` exactly, including the leading `:` /
hostname. Both tools accept the same `host:port` syntax.

---

## 6. ExtendVote takes longer than the prevote timeout

**Symptoms.** Validator emits empty vote-extensions intermittently;
cometbft logs `extending vote extension cancelled`.

**Cause.** The daemon goroutine has stalled (e.g. blocked on a hung TCP
read against the sidecar) and the cache went cold. ExtendVote falls
back to an empty payload to keep liveness.

**Fix.**

- Drop `oracle.fetch_timeout` to a value smaller than your prevote
  timeout (default 200ms).
- Ensure the sidecar has CPU headroom; a starved sidecar will often
  miss its own polling cadence and fall behind.
- Confirm you are not running the daemon and the sidecar with the same
  `localhost` interface across hosts. They MUST be on the same machine
  (or at least the same low-latency LAN segment).

---

## 7. Aggregated price has unexpected jumps

**Symptoms.** `OraclePrice.MarkPrice` swings wildly between blocks even
though external markets are stable.

**Cause.** EMA smoothing is disabled (`MarkPriceEmaAlpha = 0` or
`>= 10000`), or fewer than three providers are returning fresh data so
the median is whoever is publishing.

**Fix.**

- Set `MarkPriceEmaAlpha` to something between `2000` and `5000` (20–50%
  of the new sample weight per block) via governance.
- Bump `Params.MinVotingPowerRatio` to require a stake supermajority
  for a market.
- Bump `min_sources` in the sidecar config.
