package main

import (
	"context"
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

	// client, err := ably.NewRealtime(ably.WithKey(""))
	// if err != nil {
	// 	panic(err)
	// }
	// channel := client.Channels.Get("")

	// Listening for messages on the queue
	g.Go(worker.New(gCtx))

	err := g.Wait()
	if err != nil {
		log.Println("Error group: ", err)
	}

	log.Println("Exiting")
}
