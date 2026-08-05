// resetdemo deletes any rows under the reserved Anvil-demo chain_id
// (999000001, see main.go's demoChains) so scripts/anvil-reorg-demo.sh
// always starts from a clean slate, regardless of what block height a
// fresh Anvil fork happens to start at.
package main

import (
	"context"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

const demoChainID = 999000001

func main() {
	godotenv.Load()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `DELETE FROM blocks WHERE chain_id = $1`, demoChainID); err != nil {
		log.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM balances WHERE chain_id = $1`, demoChainID); err != nil {
		log.Fatal(err)
	}
	log.Println("demo chain_id 999000001 reset")
}
