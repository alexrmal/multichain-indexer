package main

import (
	"context"
	_ "embed"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx, so insert helpers
// work the same whether called standalone or inside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func ApplySchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schemaSQL)
	return err
}

// MaxBlockNumber returns the highest indexed block number for chainID, or 0
// if nothing has been indexed yet.
func MaxBlockNumber(ctx context.Context, q Querier, chainID int64) (int64, error) {
	var n *int64
	err := q.QueryRow(ctx, `SELECT MAX(number) FROM blocks WHERE chain_id = $1`, chainID).Scan(&n)
	if err != nil {
		return 0, err
	}
	if n == nil {
		return 0, nil
	}
	return *n, nil
}

// GetBlockHash returns the stored hash for (chainID, number), or "" if no
// such row exists.
func GetBlockHash(ctx context.Context, q Querier, chainID, number int64) (string, error) {
	var hash string
	err := q.QueryRow(ctx, `SELECT hash FROM blocks WHERE chain_id = $1 AND number = $2`, chainID, number).Scan(&hash)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return hash, err
}

func InsertBlock(ctx context.Context, q Querier, chainID, number int64, hash, parentHash string, ts time.Time) error {
	_, err := q.Exec(ctx, `
		INSERT INTO blocks (chain_id, number, hash, parent_hash, "timestamp")
		VALUES ($1, $2, $3, $4, $5)`,
		chainID, number, hash, parentHash, ts)
	return err
}

func InsertTransaction(ctx context.Context, q Querier, chainID int64, hash string, blockNumber int64, from string, to *string, valueWei string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO transactions (chain_id, hash, block_number, from_address, to_address, value_wei)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		chainID, hash, blockNumber, from, to, valueWei)
	return err
}

func UpsertBalance(ctx context.Context, q Querier, chainID int64, address, balanceWei string, updatedAtBlock int64) error {
	_, err := q.Exec(ctx, `
		INSERT INTO balances (chain_id, address, balance_wei, updated_at_block)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (chain_id, address) DO UPDATE
			SET balance_wei = EXCLUDED.balance_wei, updated_at_block = EXCLUDED.updated_at_block`,
		chainID, address, balanceWei, updatedAtBlock)
	return err
}

// AddressesAboveHeight returns the distinct from/to addresses of all
// transactions in blocks above height, so their balances can be refreshed
// before those blocks are rolled back.
func AddressesAboveHeight(ctx context.Context, q Querier, chainID, height int64) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT addr FROM (
			SELECT from_address AS addr FROM transactions WHERE chain_id = $1 AND block_number > $2
			UNION
			SELECT to_address AS addr FROM transactions WHERE chain_id = $1 AND block_number > $2 AND to_address IS NOT NULL
		) t`, chainID, height)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var addrs []string
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, err
		}
		addrs = append(addrs, addr)
	}
	return addrs, rows.Err()
}

// RollbackAbove deletes blocks (and, via ON DELETE CASCADE, their
// transactions) above forkPoint for chainID.
func RollbackAbove(ctx context.Context, q Querier, chainID, forkPoint int64) error {
	_, err := q.Exec(ctx, `DELETE FROM blocks WHERE chain_id = $1 AND number > $2`, chainID, forkPoint)
	return err
}
