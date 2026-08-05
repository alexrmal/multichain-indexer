package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
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

func main() {
	if err := godotenv.Load(); err != nil {
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

	var wg sync.WaitGroup
	for _, cfg := range chains {
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
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			idx.Run(ctx)
		}()
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
