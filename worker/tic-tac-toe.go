package worker

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/ably/ably-go/ably"
	"github.com/go-redis/redis/v8"
)

type Room struct {
	Host  *string       `json:"host"`
	State string        `json:"state"`
	Guest *string       `json:"guest"`
	Data  TicTacToeData `json:"data"`
}

type TicTacToeData struct {
	Ticks      int      `json:"ticks"`
	Board      []string `json:"board"`
	Turn       string   `json:"turn"`
	TurnEndsAt int      `json:"turn_ends_at"`
}

type Action int

const (
	HOST_CHANGE Action = iota
)

func (a Action) String() string {
	return [...]string{"HOST_CHANGE"}[a]
}

func onEnter(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	log.Printf("%s entered channel %s\n", presence.ClientId, presenceMsg.Channel)

	channel := presenceMsg.Channel
	if strings.HasPrefix(channel, "control:") {
		onControlEnter(ctx, presenceMsg)
	}
}

func onLeave(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	log.Printf("%s left channel %s\n", presence.ClientId, presenceMsg.Channel)

	channel := presenceMsg.Channel
	if strings.HasPrefix(channel, "control:") {
		onControlLeave(ctx, presenceMsg)
	}
}

func onControlEnter(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	channel := presenceMsg.Channel
	roomId := strings.Replace(channel, "control:", "", 1)
	clientId := presence.ClientId
	redisClient := ctx.Value(redisCtxKey{}).(*redis.Client)
	serverChannel := ctx.Value(serverChannelCtxKey{}).(*ably.RealtimeChannel)

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
		redisClient.Persist(ctx, "room:"+roomId)
		// Notify the client that you're now the host
		serverChannel.Publish(ctx, HOST_CHANGE.String(), clientId)
		return
	}
	// TODO: do something when the host is already set
}

func onControlLeave(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	channel := presenceMsg.Channel
	roomId := strings.Replace(channel, "control:", "", 1)
	clientId := presence.ClientId
	redisClient := ctx.Value(redisCtxKey{}).(*redis.Client)
	serverChannel := ctx.Value(serverChannelCtxKey{}).(*ably.RealtimeChannel)

	val, err := redisClient.Do(ctx, "JSON.GET", "room:"+roomId, "$").Result()
	if err != nil {
		if err == redis.Nil {
			log.Printf("Room %s not exists\n", roomId)
			return
		}
		log.Printf("Error getting room %s: %s\n", roomId, err)
		return
	}

	var data []Room
	err = json.Unmarshal([]byte(val.(string)), &data)
	if err != nil {
		log.Printf("Error unmarshalling json data: %s. Raw: %s\n", err, val.(string))
		return
	}
	room := data[0]

	switch room.State {
	case "waiting":
		if clientId == *room.Host {
			if room.Guest == nil {
				// No guest, delete room
				redisClient.Do(ctx, "JSON.DEL", "room:"+roomId)
				// Don't need to notify anyone, the room is gone
				log.Printf("Room %s deleted\n", roomId)
				return
			}
			// Make guest the new host
			pipe := redisClient.Pipeline()
			pipe.Do(ctx, "JSON.SET", "room:"+roomId, "$.host", "\""+*room.Guest+"\"")
			pipe.Do(ctx, "JSON.SET", "room:"+roomId, "$.guest", "null")
			pipe.Exec(ctx)
			// Notify the guest that you're now the host
			serverChannel.Publish(ctx, HOST_CHANGE.String(), room.Guest)
		}
	}
}
