package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// testChainID is a reserved chain ID that will never collide with a real
// network, used to isolate integration test data from real indexed rows in
// the same Postgres database.
const testChainID = 999999001

func setupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	godotenv.Load()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := ApplySchema(ctx, pool); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}

	cleanup := func() {
		pool.Exec(context.Background(), `DELETE FROM blocks WHERE chain_id = $1`, testChainID)
		pool.Exec(context.Background(), `DELETE FROM balances WHERE chain_id = $1`, testChainID)
	}
	cleanup() // in case a previous failed run left rows behind
	t.Cleanup(func() {
		cleanup()
		pool.Close()
	})
	return pool
}

func TestRollbackCascadesTransactions(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0)

	if err := InsertBlock(ctx, pool, testChainID, 1, "0xblock1", "0xgenesis", now); err != nil {
		t.Fatalf("InsertBlock 1: %v", err)
	}
	if err := InsertBlock(ctx, pool, testChainID, 2, "0xblock2", "0xblock1", now); err != nil {
		t.Fatalf("InsertBlock 2: %v", err)
	}
	to := "0xto"
	if err := InsertTransaction(ctx, pool, testChainID, "0xtx1", 2, "0xfrom", &to, "100"); err != nil {
		t.Fatalf("InsertTransaction: %v", err)
	}

	if err := RollbackAbove(ctx, pool, testChainID, 1); err != nil {
		t.Fatalf("RollbackAbove: %v", err)
	}

	maxNum, err := MaxBlockNumber(ctx, pool, testChainID)
	if err != nil {
		t.Fatalf("MaxBlockNumber: %v", err)
	}
	if maxNum != 1 {
		t.Fatalf("expected block 2 rolled back, MaxBlockNumber = %d", maxNum)
	}

	var txCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transactions WHERE chain_id = $1`, testChainID).Scan(&txCount); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if txCount != 0 {
		t.Fatalf("expected transactions cascade-deleted with their block, found %d", txCount)
	}
}

func TestUpsertBalanceUpdatesInPlace(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	addr := "0xabc"

	if err := UpsertBalance(ctx, pool, testChainID, addr, "100", 1); err != nil {
		t.Fatalf("UpsertBalance 1: %v", err)
	}
	if err := UpsertBalance(ctx, pool, testChainID, addr, "200", 2); err != nil {
		t.Fatalf("UpsertBalance 2: %v", err)
	}

	var count int
	var balance string
	err := pool.QueryRow(ctx,
		`SELECT count(*), max(balance_wei) FROM balances WHERE chain_id = $1 AND address = $2`,
		testChainID, addr,
	).Scan(&count, &balance)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row (upsert, not insert), got %d", count)
	}
	if balance != "200" {
		t.Fatalf("expected balance updated to 200, got %s", balance)
	}
}

func TestAddressesAboveHeight(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0)

	if err := InsertBlock(ctx, pool, testChainID, 1, "0xb1", "0xg", now); err != nil {
		t.Fatalf("InsertBlock 1: %v", err)
	}
	if err := InsertBlock(ctx, pool, testChainID, 2, "0xb2", "0xb1", now); err != nil {
		t.Fatalf("InsertBlock 2: %v", err)
	}
	to := "0xrecipient"
	if err := InsertTransaction(ctx, pool, testChainID, "0xtx1", 2, "0xsender", &to, "50"); err != nil {
		t.Fatalf("InsertTransaction: %v", err)
	}

	addrs, err := AddressesAboveHeight(ctx, pool, testChainID, 1)
	if err != nil {
		t.Fatalf("AddressesAboveHeight: %v", err)
	}
	got := map[string]bool{}
	for _, a := range addrs {
		got[a] = true
	}
	if !got["0xsender"] || !got["0xrecipient"] {
		t.Fatalf("expected sender+recipient in %v", addrs)
	}
}
