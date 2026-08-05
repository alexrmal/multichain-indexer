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
via `schema.sql`) — no separate migration step.

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

## Known gaps

- No WebSocket subscriptions — polling only.
- Reorg walk-back is capped at 20 blocks; a deeper reorg logs a fatal error
  requiring manual intervention.
- No historical backfill — indexing starts a few blocks behind the current
  tip, not from genesis or a configured start block.
- Balances track native-token value only (via `eth_getBalance`), not a full
  accounting ledger — no gas, logs, or ERC-20 transfer tracking.
- Hardcoded two-chain config.
- No real (Anvil-forked) reorg demo yet — only the DB-corruption method
  above; a genuinely-forked-chain demo is a stronger proof and still on the
  list.
- Not deployed anywhere yet — runs locally only.
