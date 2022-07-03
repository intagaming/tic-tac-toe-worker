package worker

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/ably/ably-go/ably"
	"github.com/go-redis/redis/v8"
	amqp "github.com/rabbitmq/amqp091-go"
)

type redisCtxKey struct{}
type ablyCtxKey struct{}

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

		// Redis
		opt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
		if err != nil {
			panic(err)
		}
		redisClient := redis.NewClient(opt)
		ctx = context.WithValue(ctx, redisCtxKey{}, redisClient)

		// Ably
		ablyClient, err := ably.NewRealtime(ably.WithKey(ablyApiKey))
		if err != nil {
			panic(err)
		}
		ctx = context.WithValue(ctx, ablyCtxKey{}, ablyClient)

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
