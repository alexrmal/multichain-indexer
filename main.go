package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

const shutdownTimeout = 10 * time.Second

type chainConfig struct {
	name          string
	chainID       int64
	rpcEnvVar     string
	startBlockVar string // optional env var naming a start block to backfill from
}

var chains = []chainConfig{
	{name: "ethereum-sepolia", chainID: 11155111, rpcEnvVar: "ETH_RPC_URL", startBlockVar: "ETH_START_BLOCK"},
	{name: "base-sepolia", chainID: 84532, rpcEnvVar: "BASE_RPC_URL", startBlockVar: "BASE_START_BLOCK"},
}

// demoChains is used only when DEMO_MODE=1, to point the indexer at a local
// Anvil fork (see scripts/anvil-reorg-demo.sh) instead of the real chains.
// It uses a reserved chain_id well outside any real network's range, and a
// separate RPC/start-block env var, so a demo run can never collide with or
// overwrite real indexed data even though it shares the same Postgres
// database and the exact same Indexer code path.
var demoChains = []chainConfig{
	{name: "anvil-fork-demo", chainID: 999000001, rpcEnvVar: "ANVIL_RPC_URL", startBlockVar: "ANVIL_START_BLOCK"},
}

// deriveWSURL turns an HTTP(S) RPC URL into the equivalent WebSocket URL,
// e.g. https://eth-sepolia.g.alchemy.com/v2/KEY -> wss://.../v2/KEY. Alchemy
// serves both protocols from the same host/path/API key, so no separate
// WebSocket secret is needed. Returns ok=false for any other scheme rather
// than guessing, since WebSocket support here is a latency optimization,
// not a requirement — the poll loop works fine without it.
func deriveWSURL(httpURL string) (string, bool) {
	switch {
	case strings.HasPrefix(httpURL, "https://"):
		return "wss://" + strings.TrimPrefix(httpURL, "https://"), true
	case strings.HasPrefix(httpURL, "http://"):
		return "ws://" + strings.TrimPrefix(httpURL, "http://"), true
	default:
		return "", false
	}
}

func main() {
	// .env is a local-dev convenience only; in deployed environments (e.g.
	// Railway) env vars are set directly by the platform and there's no
	// .env file at all, so a missing file here is not an error.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Fatal("Error loading .env file: ", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal("Failed to connect to Postgres: ", err)
	}
	defer pool.Close()

	if err := ApplySchema(ctx, pool); err != nil {
		log.Fatal("Failed to apply schema: ", err)
	}

	activeChains := chains
	if os.Getenv("DEMO_MODE") == "1" {
		activeChains = demoChains
		log.Println("DEMO_MODE=1: indexing only the local Anvil fork under a reserved demo chain_id")
	}

	var wg sync.WaitGroup
	for _, cfg := range activeChains {
		rpcURL := os.Getenv(cfg.rpcEnvVar)
		if rpcURL == "" {
			log.Fatalf("%s is not set", cfg.rpcEnvVar)
		}

		client, err := ethclient.Dial(rpcURL)
		if err != nil {
			log.Fatalf("Failed to connect to %s: %v", cfg.name, err)
		}

		var startBlock int64
		if v := os.Getenv(cfg.startBlockVar); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				log.Fatalf("%s must be an integer, got %q: %v", cfg.startBlockVar, v, err)
			}
			startBlock = n
		}

		idx := &Indexer{
			Name:       cfg.name,
			ChainID:    cfg.chainID,
			Client:     client,
			Pool:       pool,
			StartBlock: startBlock,
			wake:       make(chan struct{}, 1),
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			idx.Run(ctx)
		}()

		if wsURL, ok := deriveWSURL(rpcURL); ok {
			wsClient, err := ethclient.DialContext(ctx, wsURL)
			if err != nil {
				log.Printf("[%s] websocket dial failed, continuing on polling only: %v", cfg.name, err)
			} else {
				wg.Add(1)
				go func() {
					defer wg.Done()
					idx.watchNewHeads(ctx, wsClient)
				}()
			}
		}
	}

	<-ctx.Done()
	log.Println("shutdown signal received, waiting for in-flight work to finish...")

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("shutdown complete")
	case <-time.After(shutdownTimeout):
		log.Println("shutdown timed out, exiting anyway")
	}
}
