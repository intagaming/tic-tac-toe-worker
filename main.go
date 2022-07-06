package main

import (
	"context"
	"github.com/ably/ably-go/ably"
	"github.com/go-redis/redis/v8"
	"hxann.com/tic-tac-toe-worker/shared"
	"hxann.com/tic-tac-toe-worker/ticker"
	"log"
	"os"
	"os/signal"
	"syscall"

	// "github.com/ably/ably-go/ably"

	"golang.org/x/sync/errgroup"
	"hxann.com/tic-tac-toe-worker/worker"
)

func main() {
	log.Println("Starting")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, gCtx := errgroup.WithContext(ctx)

	// Redis
	opt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
	if err != nil {
		panic(err)
	}
	redisClient := redis.NewClient(opt)
	gCtx = context.WithValue(gCtx, shared.RedisCtxKey{}, redisClient)

	// Ably
	ablyApiKey := os.Getenv("ABLY_API_KEY")
	ablyClient, err := ably.NewRealtime(ably.WithKey(ablyApiKey))
	if err != nil {
		panic(err)
	}
	gCtx = context.WithValue(gCtx, shared.AblyCtxKey{}, ablyClient)

	// Worker listening for messages on the queue
	g.Go(worker.New(gCtx))

	// New ticker
	g.Go(ticker.New(gCtx))

	err = g.Wait()
	if err != nil {
		log.Println("Error group: ", err)
	}

	log.Println("Exiting")
}
