package worker

import (
	"context"
	"fmt"
	"log"
	"os"

	amqp "github.com/rabbitmq/amqp091-go"
)

func New(ctx context.Context) func() error {
	return func() error {
		ablyApiKey := os.Getenv("ABLY_API_KEY")
		ablyQueueName := os.Getenv("ABLY_QUEUE_NAME")

		conn, err := amqp.Dial(fmt.Sprintf("amqps://%s@us-east-1-a-queue.ably.io:5671/shared", ablyApiKey))
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
			ablyQueueName, // queue
			"",            // consumer
			true,          // auto-ack
			false,         // exclusive
			false,         // no-local
			false,         // no-wait
			nil,           // args
		)
		if err != nil {
			log.Panicf("Failed to consume messages: %s", err)
		}
		log.Println("Listening for messages on queue")

		for {
			select {
			case <-ctx.Done():
				fmt.Println("Breaking out of the loop")
				return nil
			case d := <-msgs:
				handle(ctx, d.Body)
			}
		}
	}
}
