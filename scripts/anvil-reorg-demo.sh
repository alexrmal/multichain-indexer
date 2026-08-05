#!/usr/bin/env bash
# Forks real Ethereum Sepolia locally with Anvil, points the indexer at it
# under a reserved demo chain_id (999000001, never collides with real
# indexed data), then uses Anvil's evm_snapshot/evm_revert to produce two
# genuinely different real forks at the same block heights -- an actual
# reorg with two real, differently-hashed chains, not a hand-edited
# database row. Confirms the indexer detects and heals it.
set -euo pipefail

export PATH="$HOME/.foundry/bin:$PATH"

command -v anvil >/dev/null || {
  echo "anvil not found. Install: curl -L https://foundry.paradigm.xyz | bash && foundryup"
  exit 1
}

cd "$(dirname "$0")/.."

ETH_RPC_URL=$(grep '^ETH_RPC_URL=' .env | cut -d= -f2-)
RPC=http://127.0.0.1:8545
ANVIL_LOG=/tmp/anvil-reorg-demo-anvil.log
INDEXER_LOG=/tmp/anvil-reorg-demo-indexer.log
INDEXER_BIN=/tmp/anvil-reorg-demo-indexer

echo "==> resetting demo chain_id to a clean slate"
go run ./scripts/resetdemo

echo "==> starting anvil, forking real Ethereum Sepolia"
anvil --fork-url "$ETH_RPC_URL" --port 8545 >"$ANVIL_LOG" 2>&1 &
ANVIL_PID=$!
INDEXER_PID=""
cleanup() {
  [ -n "$INDEXER_PID" ] && kill "$INDEXER_PID" 2>/dev/null || true
  kill "$ANVIL_PID" 2>/dev/null || true
}
trap cleanup EXIT
sleep 3

FORK_BLOCK=$(cast block-number --rpc-url "$RPC")
echo "==> forked at block $FORK_BLOCK"

echo "==> building and starting the demo indexer against the fork"
go build -o "$INDEXER_BIN" .
DEMO_MODE=1 ANVIL_RPC_URL="$RPC" ANVIL_START_BLOCK="$FORK_BLOCK" "$INDEXER_BIN" >"$INDEXER_LOG" 2>&1 &
INDEXER_PID=$!

wait_for_log() {
  local pattern=$1 timeout=$2 waited=0
  while ! grep -q "$pattern" "$INDEXER_LOG" 2>/dev/null; do
    sleep 1
    waited=$((waited + 1))
    if [ "$waited" -ge "$timeout" ]; then
      echo "timed out waiting for: $pattern"
      cat "$INDEXER_LOG"
      exit 1
    fi
  done
}

echo "==> waiting for the fork block to be indexed"
wait_for_log "indexed block $FORK_BLOCK" 30

SNAP=$(cast rpc evm_snapshot --rpc-url "$RPC" | tr -d '"')
SENDER_KEY=0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80
ACCOUNT_A=0x70997970C51812dc3A010C7d01b50e0d17dc79C8
ACCOUNT_B=0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC

echo "==> mining branch A: 2 blocks"
cast send --rpc-url "$RPC" --private-key "$SENDER_KEY" "$ACCOUNT_A" --value 1ether >/dev/null
cast send --rpc-url "$RPC" --private-key "$SENDER_KEY" "$ACCOUNT_A" --value 1ether >/dev/null
BRANCH_A_TIP=$((FORK_BLOCK + 2))

echo "==> waiting for branch A to be indexed"
wait_for_log "indexed block $BRANCH_A_TIP" 30

echo "==> reverting anvil to the pre-branch-A snapshot"
cast rpc evm_revert "$SNAP" --rpc-url "$RPC" >/dev/null

echo "==> mining branch B: 4 different blocks (different recipient/amounts -> genuinely different hashes)"
for i in 1 2 3 4; do
  cast send --rpc-url "$RPC" --private-key "$SENDER_KEY" "$ACCOUNT_B" --value "${i}ether" >/dev/null
done

echo "==> waiting for the indexer to detect and heal the reorg"
wait_for_log "reorg detected" 30
wait_for_log "rolled back to block $FORK_BLOCK" 10

echo
echo "=== reorg detected and healed against two real forks ==="
grep -E "indexed block|reorg detected|rolled back" "$INDEXER_LOG"
echo "=========================================================="
echo "full indexer log: $INDEXER_LOG"
echo "full anvil log:   $ANVIL_LOG"
