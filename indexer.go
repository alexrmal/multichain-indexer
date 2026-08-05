package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rawTransaction/rawBlock decode only the fields we need directly from the
// eth_getBlockByNumber JSON response, bypassing go-ethereum's typed
// transaction decoder. Base (and other OP-stack chains) include deposit
// transactions (type 0x7E) that go-ethereum's standard decoder rejects,
// which would otherwise fail the whole block. Decoding "from" straight out
// of the JSON also means we don't need an extra TransactionSender RPC call
// per transaction — Alchemy already includes it.
type rawTransaction struct {
	Hash  common.Hash     `json:"hash"`
	From  common.Address  `json:"from"`
	To    *common.Address `json:"to"`
	Value *hexutil.Big    `json:"value"`
}

type rawBlock struct {
	Hash         common.Hash      `json:"hash"`
	ParentHash   common.Hash      `json:"parentHash"`
	Timestamp    hexutil.Uint64   `json:"timestamp"`
	Transactions []rawTransaction `json:"transactions"`
}

func fetchBlock(ctx context.Context, client *ethclient.Client, number int64) (*rawBlock, error) {
	var blk rawBlock
	err := client.Client().CallContext(ctx, &blk, "eth_getBlockByNumber", hexutil.EncodeBig(big.NewInt(number)), true)
	if err != nil {
		return nil, err
	}
	if blk.Hash == (common.Hash{}) {
		return nil, fmt.Errorf("block %d not found", number)
	}
	return &blk, nil
}

const (
	pollInterval  = 3 * time.Second
	backfill      = 5  // blocks to seed on first run so there's local history to demo reorg against
	maxReorgDepth = 20 // walk-back cap; deeper reorgs need manual intervention
)

type Indexer struct {
	Name    string
	ChainID int64
	Client  *ethclient.Client
	Pool    *pgxpool.Pool
}

func (idx *Indexer) Run(ctx context.Context) {
	for {
		advanced, err := idx.tick(ctx)
		if err != nil {
			log.Printf("[%s] error: %v", idx.Name, err)
			time.Sleep(pollInterval)
			continue
		}
		if !advanced {
			time.Sleep(pollInterval)
		}
	}
}

// tick indexes at most one block. It returns advanced=true if it made
// progress (indexed a block or rolled back a reorg), so Run can immediately
// try to catch up further without waiting out a full poll interval.
func (idx *Indexer) tick(ctx context.Context) (bool, error) {
	remoteLatest, err := idx.Client.BlockNumber(ctx)
	if err != nil {
		return false, fmt.Errorf("BlockNumber: %w", err)
	}

	lastLocal, err := MaxBlockNumber(ctx, idx.Pool, idx.ChainID)
	if err != nil {
		return false, fmt.Errorf("MaxBlockNumber: %w", err)
	}

	if lastLocal == 0 {
		seed := int64(remoteLatest) - backfill
		if seed < 0 {
			seed = 0
		}
		lastLocal = seed
	}

	if int64(remoteLatest) <= lastLocal {
		return false, nil
	}

	next := lastLocal + 1
	block, err := fetchBlock(ctx, idx.Client, next)
	if err != nil {
		return false, fmt.Errorf("fetchBlock(%d): %w", next, err)
	}

	if lastLocal > 0 {
		storedHash, err := GetBlockHash(ctx, idx.Pool, idx.ChainID, lastLocal)
		if err != nil {
			return false, fmt.Errorf("GetBlockHash: %w", err)
		}
		if storedHash != "" && block.ParentHash.Hex() != storedHash {
			log.Printf("[%s] reorg detected: incoming block %d has parent %s, stored block %d has hash %s",
				idx.Name, next, block.ParentHash.Hex(), lastLocal, storedHash)

			forkPoint, err := idx.findForkPoint(ctx, lastLocal)
			if err != nil {
				return false, fmt.Errorf("findForkPoint: %w", err)
			}
			if err := idx.rollback(ctx, forkPoint); err != nil {
				return false, fmt.Errorf("rollback: %w", err)
			}
			log.Printf("[%s] rolled back to block %d, re-indexing from there", idx.Name, forkPoint)
			return true, nil
		}
	}

	if err := idx.indexBlock(ctx, next, block); err != nil {
		return false, fmt.Errorf("indexBlock(%d): %w", next, err)
	}
	log.Printf("[%s] indexed block %d (%d txs)", idx.Name, next, len(block.Transactions))
	return true, nil
}

// findForkPoint walks backward from `from`, comparing the locally stored
// hash at each height against the chain's real hash, until it finds the
// last common ancestor.
func (idx *Indexer) findForkPoint(ctx context.Context, from int64) (int64, error) {
	n := from
	for depth := 0; depth < maxReorgDepth; depth++ {
		if n == 0 {
			return 0, nil
		}
		header, err := idx.Client.HeaderByNumber(ctx, big.NewInt(n))
		if err != nil {
			return 0, fmt.Errorf("HeaderByNumber(%d): %w", n, err)
		}
		localHash, err := GetBlockHash(ctx, idx.Pool, idx.ChainID, n)
		if err != nil {
			return 0, err
		}
		if header.Hash().Hex() == localHash {
			return n, nil
		}
		n--
	}
	return 0, fmt.Errorf("reorg deeper than %d blocks, manual intervention needed", maxReorgDepth)
}

// rollback removes every block above forkPoint (transactions cascade), then
// refreshes balances for addresses touched by the orphaned blocks so they
// don't keep showing stale balances even if they never reappear on the
// canonical chain.
func (idx *Indexer) rollback(ctx context.Context, forkPoint int64) error {
	addrs, err := AddressesAboveHeight(ctx, idx.Pool, idx.ChainID, forkPoint)
	if err != nil {
		return fmt.Errorf("AddressesAboveHeight: %w", err)
	}

	if err := RollbackAbove(ctx, idx.Pool, idx.ChainID, forkPoint); err != nil {
		return fmt.Errorf("RollbackAbove: %w", err)
	}

	for _, addr := range addrs {
		if err := idx.refreshBalance(ctx, addr, forkPoint); err != nil {
			log.Printf("[%s] failed to refresh balance for %s: %v", idx.Name, addr, err)
		}
	}
	return nil
}

func (idx *Indexer) indexBlock(ctx context.Context, number int64, block *rawBlock) error {
	tx, err := idx.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("Begin: %w", err)
	}
	defer tx.Rollback(ctx)

	ts := time.Unix(int64(block.Timestamp), 0)

	if err := InsertBlock(ctx, tx, idx.ChainID, number, block.Hash.Hex(), block.ParentHash.Hex(), ts); err != nil {
		return fmt.Errorf("InsertBlock: %w", err)
	}

	addrSet := make(map[string]struct{})
	for _, txn := range block.Transactions {
		from := txn.From.Hex()

		var to *string
		if txn.To != nil {
			s := txn.To.Hex()
			to = &s
		}

		valueWei := "0"
		if txn.Value != nil {
			valueWei = txn.Value.ToInt().String()
		}

		if err := InsertTransaction(ctx, tx, idx.ChainID, txn.Hash.Hex(), number, from, to, valueWei); err != nil {
			return fmt.Errorf("InsertTransaction: %w", err)
		}

		addrSet[from] = struct{}{}
		if to != nil {
			addrSet[*to] = struct{}{}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("Commit: %w", err)
	}

	for addr := range addrSet {
		if err := idx.refreshBalance(ctx, addr, number); err != nil {
			log.Printf("[%s] failed to refresh balance for %s: %v", idx.Name, addr, err)
		}
	}
	return nil
}

func (idx *Indexer) refreshBalance(ctx context.Context, addr string, atBlock int64) error {
	bal, err := idx.Client.BalanceAt(ctx, common.HexToAddress(addr), nil)
	if err != nil {
		return err
	}
	return UpsertBalance(ctx, idx.Pool, idx.ChainID, addr, bal.String(), atBlock)
}
