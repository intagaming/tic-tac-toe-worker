package worker

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/ably/ably-go/ably"
	"github.com/go-redis/redis/v8"
)

type Action int

const (
	HOST_IS_SELF Action = iota
)

func (a Action) String() string {
	return [...]string{"HOST_IS_SELF"}[a]
}

func onEnter(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	log.Printf("%s entered channel %s\n", presence.ClientId, presenceMsg.Channel)

	channel := presenceMsg.Channel
	if strings.HasPrefix(channel, "control:") {
		onControlEnter(ctx, presenceMsg)
	}
}

func onControlEnter(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	channel := presenceMsg.Channel
	roomId := strings.Replace(channel, "control:", "", 1)
	clientId := presence.ClientId
	redisClient := ctx.Value(redisCtxKey{}).(*redis.Client)
	ablyClient := ctx.Value(ablyCtxKey{}).(*ably.Realtime)

	val, err := redisClient.Do(ctx, "JSON.GET", "room:"+roomId, "$.host").Result()
	if err != nil {
		if err == redis.Nil {
			log.Printf("Room %s not exists\n", roomId)
			return
		}
		log.Printf("Error getting room %s: %s\n", roomId, err)
		return
	}
	if val == "[null]" {
		// Set as host
		// TODO: fix race condition when we're setting the host but someone else joins first and became the host?
		redisClient.Do(ctx, "JSON.SET", "room:"+roomId, "$.host", "\""+clientId+"\"")
		redisClient.Expire(ctx, "room:"+roomId, 300*time.Second) // TODO: in reality this should be -1. Implement leave logic first.
		serverChannel := ablyClient.Channels.Get("server:" + roomId)
		serverChannel.Publish(ctx, HOST_IS_SELF.String(), nil)
		return
	}
	// TODO: do something when the host is already set
}
