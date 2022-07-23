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
		ablyQueueName := os.Getenv("ABLY_QUEUE_NAME")
		amqpUrl := os.Getenv("AMQP_URL")

		conn, err := amqp.Dial(amqpUrl)
		if err != nil {
			log.Panicf("Error connecting to RabbitMQ: %s", err)
		}
		defer func(conn *amqp.Connection) {
			err := conn.Close()
			if err != nil {
				log.Panicf("Error closing connection: %s", err)
			}
		}(conn)
		log.Println("Connected to Ably queue")

		ch, err := conn.Channel()
		if err != nil {
			log.Panicf("Failed to open a channel: %s", err)
		}
		defer func(ch *amqp.Channel) {
			err := ch.Close()
			if err != nil {
				log.Panicf("Error closing channel: %s", err)
			}
		}(ch)
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
