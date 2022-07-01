package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	// "github.com/ably/ably-go/ably"
	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// client, err := ably.NewRealtime(ably.WithKey("pb5aMA.u7Kfiw:bJjpRT-5Gsj8pG8iAzlsPGeapP1MzB18sRzYxmgUWwg"))
	// if err != nil {
	// 	panic(err)
	// }
	// channel := client.Channels.Get("")

	// Listening for messages on the queue
	conn, err := amqp.Dial("amqps://pb5aMA.u7Kfiw:bJjpRT-5Gsj8pG8iAzlsPGeapP1MzB18sRzYxmgUWwg@us-east-1-a-queue.ably.io:5671/shared")
	if err != nil {
		log.Panicf("Error connecting to RabbitMQ: %s", err)
	}
	defer conn.Close()
	log.Println("Connected to Ably queue")

	ch, err := conn.Channel()
	if err != nil {
		log.Panicf("Failed to open a channel: %s", err)
	}
	defer ch.Close()
	log.Println("Opened a channel on the Ably queue connection")

	msgs, err := ch.Consume(
		"pb5aMA:tictactoe", // queue
		"",                 // consumer
		true,               // auto-ack
		false,              // exclusive
		false,              // no-local
		false,              // no-wait
		nil,                // args
	)
	if err != nil {
		log.Panicf("Failed to consume messages: %s", err)
	}
	log.Println("Listening for messages on queue")

	wg.Add(1)
	go func() {
		defer wg.Done()
	out:
		for {
			select {
			case <-ctx.Done():
				fmt.Println("Breaking out of the loop")
				break out // Break the for loop
			case d := <-msgs:
				fmt.Printf("Delivered message: %s\n", d.Body)
			}
		}
	}()

	wg.Wait()
}
