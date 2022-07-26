package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ably/ably-go/ably"
	"github.com/go-redis/redis/v8"
	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v8"
	"github.com/joho/godotenv"
	"golang.org/x/sync/errgroup"
	"hxann.com/tic-tac-toe-worker/shared"
	"hxann.com/tic-tac-toe-worker/ticker"
	"hxann.com/tic-tac-toe-worker/worker"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("Not loading .env file. Assuming env vars are available.")
	}

	log.Println("Starting")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, gCtx := errgroup.WithContext(ctx)

	// Redis
	opt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
	if err != nil {
		panic(err)
	}
	rdb := redis.NewClient(opt)
	gCtx = context.WithValue(gCtx, shared.RedisCtxKey{}, rdb)

	// Ably
	ablyApiKey := os.Getenv("ABLY_API_KEY")
	ablyClient, err := ably.NewRealtime(ably.WithKey(ablyApiKey))
	if err != nil {
		panic(err)
	}
	gCtx = context.WithValue(gCtx, shared.AblyCtxKey{}, ablyClient)

	pool := goredis.NewPool(rdb)
	rs := redsync.New(pool)
	gCtx = context.WithValue(gCtx, shared.RedsyncCtxKey{}, rs)

	// Starting workers for listening for messages on the queue
	// Getting number of workers
	numWorkersStr := os.Getenv("NUMBER_OF_WORKERS")
	numWorkers, err := strconv.Atoi(numWorkersStr)
	if err != nil {
		log.Println("Could not parse number of workers. Using default value of 1.")
		numWorkers = 1
	}
	// Start workers
	for i := 0; i < numWorkers; i++ {
		g.Go(worker.New(gCtx))
	}

	// Starting tickers for ticking the rooms
	// Getting number of tickers
	numTickersStr := os.Getenv("NUMBER_OF_TICKERS")
	numTickers, err := strconv.Atoi(numTickersStr)
	if err != nil {
		log.Println("Could not parse number of tickers. Using default value of 1.")
		numTickers = 1
	}
	// Start tickers
	for i := 0; i < numTickers; i++ {
		g.Go(ticker.New(gCtx))
	}

	err = g.Wait()
	if err != nil {
		log.Println("Error group: ", err)
	}

	log.Println("Exiting")
}
