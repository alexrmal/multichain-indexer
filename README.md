# multichain-indexer

A minimal blockchain indexer for Ethereum Sepolia and Base Sepolia that
detects and recovers from chain reorganizations. The indexing pipeline is
intentionally thin — reorg detection and rollback is the point of this
project, not a full-featured indexer.

## What it does

- Polls both chains (via Alchemy) for new blocks, one goroutine per chain.
- Stores blocks, transactions, and native-token wallet balances in Postgres.
- On every new block, checks its `parentHash` against the hash already
  stored for the previous block. A mismatch means the chain reorganized
  since the last poll.
- On a detected reorg: walks backward to find the last block where the
  locally stored hash still matches the chain, deletes everything indexed
  above that fork point, refreshes balances for any address affected by the
  orphaned blocks, and resumes indexing forward from the fork point on the
  new canonical chain.

## Reorg detection and recovery

A reorg happens when the chain your node/RPC provider considers canonical
changes — blocks you already indexed get orphaned and replaced by a
different set of blocks at the same heights. An indexer that doesn't handle
this ends up with stale or duplicate data forever.

**Detection.** Each stored block records its own hash and its parent's hash.
Before indexing block `N`, the indexer compares block `N`'s `parentHash`
against the hash it has stored locally for block `N-1`. If they don't match,
the chain has reorganized somewhere at or before `N-1`.

**Finding the fork point.** From `N-1`, the indexer walks backward one block
at a time, re-fetching each height's real hash from the chain and comparing
it to what's stored locally, until it finds a height where they agree. That
height is the fork point — the last block both chains still share.

**Rolling back.** Everything stored above the fork point belongs to the
orphaned fork, so it's deleted:

```sql
DELETE FROM blocks WHERE chain_id = $1 AND number > $2;
```

`transactions` has a foreign key to `blocks` with `ON DELETE CASCADE`, so
deleting a block row automatically deletes its transactions — no separate
cleanup query needed. `balances` is *not* tied to `blocks` by foreign key: it
stores current balances, not a history, so before the delete runs, the
indexer collects every address that appeared in a transaction in the blocks
about to be orphaned and re-fetches their live balance directly from the
chain. This matters because a wallet that only received funds in an orphaned
block would otherwise keep showing that stale balance forever, even after
the block itself is gone.

**Resuming.** After rollback, the indexer's "last indexed block" is just
`MAX(number)` for that chain — which is now the fork point — so the normal
polling loop picks back up from there and re-indexes forward along the new
canonical chain, with no separate resume logic required.

See [`indexer.go`](./indexer.go) for the implementation
(`tick`, `findForkPoint`, `rollback`).

### Demoing it without a real reorg

Testnets don't reorg on command. To prove the detection/rollback path works
against real chain data rather than mocked inputs, corrupt one row in
Postgres to simulate what a real reorg leaves behind — a stored hash that no
longer matches reality:

```sql
UPDATE blocks SET hash = '0xdead...' WHERE chain_id = <chain_id> AND number = <current_tip>;
```

On its next poll tick, the next real block's `parentHash` won't match the
now-fake stored hash, which triggers the exact same detection → walk-back →
rollback → re-index path a genuine reorg would. Confirmed working:

```
reorg detected: incoming block 45060718 has parent 0x718a8c...204f, stored block 45060717 has hash 0xdead...
rolled back to block 45060716, re-indexing from there
indexed block 45060717 (33 txs)
indexed block 45060718 (31 txs)
```

### A real reorg, via a local Anvil fork

The DB-corruption method above proves the code path fires on a mismatched
hash, but it doesn't prove the indexer heals a reorg against two *actually
different real chains*. `scripts/anvil-reorg-demo.sh` does that: it forks
real Ethereum Sepolia locally with [Anvil](https://book.getfoundry.sh/anvil/),
points the indexer at the fork under a reserved demo `chain_id` (`999000001`
— see `demoChains` in `main.go`, never touches real indexed data), then uses
Anvil's `evm_snapshot`/`evm_revert` to mine one set of blocks, revert, and
mine a *different* set of blocks at the same heights — two genuinely
different, really-hashed forks, not an edited database row.

```sh
./scripts/anvil-reorg-demo.sh
```

Installs nothing itself — install Foundry first if you don't have it:
`curl -L https://foundry.paradigm.xyz | bash && foundryup`. Actual output
from a run:

```
==> forked at block 11421130
==> mining branch A: 2 blocks
==> reverting anvil to the pre-branch-A snapshot
==> mining branch B: 4 different blocks (different recipient/amounts -> genuinely different hashes)

=== reorg detected and healed against two real forks ===
indexed block 11421130 (109 txs)
indexed block 11421131 (1 txs)
indexed block 11421132 (1 txs)
reorg detected: incoming block 11421133 has parent 0x883c38dd..., stored block 11421132 has hash 0x45aa3adf...
rolled back to block 11421130, re-indexing from there
indexed block 11421131 (1 txs)
indexed block 11421132 (1 txs)
indexed block 11421133 (1 txs)
```

Verified directly against Postgres afterward, too: the re-indexed blocks'
hashes match branch B's real `cast send` receipts exactly, not branch A's.

## Schema

```sql
blocks (chain_id, number) PRIMARY KEY, hash, parent_hash, timestamp
transactions (chain_id, hash) PRIMARY KEY, block_number -> blocks, from_address, to_address, value_wei
balances (chain_id, address) PRIMARY KEY, balance_wei, updated_at_block
```

`blocks` is keyed by `(chain_id, number)` rather than a surrogate ID so
rollback and resume can both operate directly on block height with no joins.

## Running it

Requires a `.env` file with:

```
ETH_RPC_URL=<Alchemy Ethereum Sepolia URL>
BASE_RPC_URL=<Alchemy Base Sepolia URL>
DATABASE_URL=<Postgres connection string>
```

```sh
go run .
```

The schema is created automatically on startup (`CREATE TABLE IF NOT EXISTS`
via `schema.sql`) — no separate migration step. Locally, `.env` is loaded if
present but not required; in a deployed environment (see below) env vars
come from the platform directly, with no `.env` file at all.

## Deployment

Deployed on [Railway](https://railway.app) as a background worker (no
inbound HTTP, purely outbound to Alchemy + Neon, so no port/health-check
requirement — this ruled out Render's free tier, which is web-service-only).
Builds straight from the checked-in `Dockerfile` (multi-stage: `golang`
build stage, `debian-slim` runtime with `ca-certificates` for TLS). Env vars
(`ETH_RPC_URL`, `BASE_RPC_URL`, `DATABASE_URL`) are set directly on Railway,
never in the image or the repo.

`SIGTERM` handling (see Graceful shutdown, above) matters specifically here:
Railway sends `SIGTERM` on every redeploy/restart, so a clean, prompt exit
was a real prerequisite for this, not just a nice-to-have.

Verified running continuously, not just "deployed once": watched
`railway logs` and separately queried Postgres directly for both chains'
max indexed block height a minute apart, confirming new blocks kept landing
the whole time, sourced from the Railway container rather than a local run.

## Tests

```sh
go test ./...
```

- `indexer_test.go` unit-tests the fork-point walk-back algorithm
  (`findForkPointFrom`) in isolation — no chain, no database, just closures
  — covering no-reorg, shallow reorg, a reorg exactly at the depth cap, a
  reorg past the depth cap, and the walk-back-to-genesis edge case. This is
  the core correctness claim of the project, so it's the most thoroughly
  tested piece.
- `db_test.go` integration-tests the Postgres layer directly (cascade delete
  on rollback, balance upsert-in-place, address collection for a rolled-back
  range) against a real database under a reserved test `chain_id`, so it
  proves the actual SQL behavior rather than mocking it. Skips automatically
  if `DATABASE_URL` isn't set.

## Retries

RPC calls (`BlockNumber`, block fetches, header lookups, balance lookups)
and DB writes (`indexBlock`, `rollback`, balance upserts) are wrapped in
`withRetry` (`retry.go`) — capped exponential backoff, context-aware so a
shutdown signal aborts a retry loop rather than waiting it out. DB retries
wrap whole units of work (an entire `Begin`-to-`Commit` transaction, not a
statement inside one already open) — retrying a single statement after a
transient failure is wrong, since Postgres has already aborted that
transaction server-side; the only correct retry is redoing the operation
from a fresh transaction. Deliberately not doing: distinguishing retryable
errors (a 429) from non-retryable ones (a malformed request) — everything
gets retried up to the cap, then surfaces as a logged error for the
existing poll loop to pick up next tick.

## Graceful shutdown

`SIGINT`/`SIGTERM` cancel a shared `context.Context` (via
`signal.NotifyContext` in `main.go`), which aborts any in-flight RPC/DB
retry loop immediately rather than waiting out the rest of a poll interval
or a retry backoff. `main()` waits for both chain goroutines to exit (with a
10s bounded timeout as a safety net) before returning.

Worth noting: block+transaction inserts were *already* atomic before this
(`indexBlock` uses one Postgres transaction, and Postgres never applies an
uncommitted transaction from a connection that just disappears) — a signal
arriving mid-`indexBlock` cancels that transaction, it's never partially
persisted. So this change is about exiting promptly, not fixing a
consistency bug. One accepted gap: the per-address balance refresh after a
block commits is a loop of independent calls, not itself transactional — a
signal mid-loop can leave a few balances stale until the next tick, which is
fine since balances are a self-healing cache, not the correctness-critical
ledger.

## Historical backfill

Optional `ETH_START_BLOCK` / `BASE_START_BLOCK` env vars let a chain start
indexing from a specific height instead of a few blocks behind the current
tip — but only the first time (when that chain has zero rows locally); it
has no effect on a chain that's already been indexed at all, so it can't be
used to force a rewind. Stated plainly: this makes the indexer *able* to
resume from an arbitrary height, at the same one-block-at-a-time pace as
live indexing — there's no batching, so pointing it at a start block
thousands of blocks back will work but will be slow and rate-limit-prone.
This is "configurable start height," not "fast historical sync."

## WebSocket subscriptions

Alongside the poll loop, a second goroutine per chain dials the same
Alchemy endpoint's WebSocket URL (derived from the HTTPS URL by swapping the
scheme — `deriveWSURL` in `main.go` — same API key, no new secret) and calls
`SubscribeNewHead`. Each new header does a non-blocking send on a small
`wake` channel that the poll loop's wait (`sleepOrDone` in `indexer.go`) also
selects on, so a new block is noticed within milliseconds instead of waiting
out the rest of `pollInterval`.

This is deliberately a **supplement, not a replacement**: `tick()` — the
actual fetch, parent-hash check, and index/rollback logic — is completely
unchanged and is still the only code path that decides what gets written to
Postgres. WebSocket delivery only changes *when* the poll loop wakes up, not
what it does. If the socket drops, `watchNewHeads` logs it and retries with
backoff; the poll loop keeps working as the fallback regardless. Verified
live: both chains' subscriptions connected (`websocket subscription active`)
and kept indexing normally for 30s with zero drops, then shut down cleanly
on SIGINT alongside the poll goroutines.

Named scope limit: a "fully event-driven" indexer would consume the header
data delivered over the socket directly and skip the redundant
`BlockNumber` poll call entirely — but that means correctly handling
out-of-order headers, duplicates, and gaps during a reconnect, which is
meaningfully harder to get right. This sidesteps all of that by treating the
WebSocket purely as a latency optimization on top of the already-correct
poll path. Accurate framing: "uses a WebSocket subscription to reduce
detection latency," not "fully event-driven ingestion."

## Known gaps

- Reorg walk-back is capped at 20 blocks; a deeper reorg logs a fatal error
  requiring manual intervention.
- Balances track native-token value only (via `eth_getBalance`), not a full
  accounting ledger — no gas, logs, or ERC-20 transfer tracking.
- Hardcoded two-chain config.
