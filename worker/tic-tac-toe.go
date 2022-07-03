package worker

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/ably/ably-go/ably"
	"github.com/go-redis/redis/v8"
)

type Room struct {
	Id    string        `json:"id"`
	Host  *string       `json:"host"`
	State string        `json:"state"`
	Guest *string       `json:"guest"`
	Data  TicTacToeData `json:"data"`
}

type TicTacToeData struct {
	Ticks      int      `json:"ticks"`
	Board      []string `json:"board"`
	Turn       string   `json:"turn"`
	TurnEndsAt int      `json:"turnEndsAt"`
}

type Action int

const (
	HOST_CHANGE Action = iota
	ROOM_STATE
)

func (a Action) String() string {
	return [...]string{"HOST_CHANGE", "ROOM_STATE"}[a]
}

func withServerChannel(ctx context.Context, channel string) context.Context {
	ablyClient := ctx.Value(ablyCtxKey{}).(*ably.Realtime)
	serverChannel := ablyClient.Channels.Get("server:" + strings.Replace(channel, "control:", "", 1))
	return context.WithValue(ctx, serverChannelCtxKey{}, serverChannel)
}

func onEnter(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	log.Printf("%s entered channel %s\n", presence.ClientId, presenceMsg.Channel)

	channel := presenceMsg.Channel
	if strings.HasPrefix(channel, "control:") {
		ctx = withServerChannel(ctx, channel)
		onControlChannelEnter(ctx, presenceMsg)
	}
}

func onLeave(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	log.Printf("%s left channel %s\n", presence.ClientId, presenceMsg.Channel)

	channel := presenceMsg.Channel
	if strings.HasPrefix(channel, "control:") {
		ctx = withServerChannel(ctx, channel)
		onControlChannelLeave(ctx, presenceMsg)
	}
}

func onControlChannelEnter(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	channel := presenceMsg.Channel
	roomId := strings.Split(strings.Replace(channel, "control:", "", 1), ":")[0]
	clientId := presence.ClientId
	redisClient := ctx.Value(redisCtxKey{}).(*redis.Client)
	serverChannel := ctx.Value(serverChannelCtxKey{}).(*ably.RealtimeChannel)

	// Get room from redis
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

	if room.Host == nil { // If no host set
		// Set as host
		// TODO: fix race condition when we're setting the host but someone else joins first and became the host?
		redisClient.Do(ctx, "JSON.SET", "room:"+roomId, "$.host", "\""+clientId+"\"")
		redisClient.Set(ctx, "client:"+clientId, roomId, 0)
		redisClient.Persist(ctx, "room:"+roomId)
		room.Host = &clientId

		// Send the room state
		roomJson, err := json.Marshal(room)
		if err != nil {
			log.Printf("Error marshalling room: %s\n", err)
			return
		}
		serverChannel.Publish(ctx, ROOM_STATE.String(), string(roomJson))
		return
	}
	// TODO: do something when the host is already set
	// Persists the client's roomId
	redisClient.Set(ctx, "client:"+clientId, roomId, 0)
	// Persists the room
	redisClient.Persist(ctx, "room:"+roomId)

	// Send the room state
	roomJson, err := json.Marshal(room)
	if err != nil {
		log.Printf("Error marshalling room: %s\n", err)
		return
	}
	serverChannel.Publish(ctx, ROOM_STATE.String(), string(roomJson))
}

func onControlChannelLeave(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	channel := presenceMsg.Channel
	roomId := strings.Split(strings.Replace(channel, "control:", "", 1), ":")[0]
	clientId := presence.ClientId
	redisClient := ctx.Value(redisCtxKey{}).(*redis.Client)
	// serverChannel := ctx.Value(serverChannelCtxKey{}).(*ably.RealtimeChannel)

	// The client has 2 hours to join the room again
	redisClient.Expire(ctx, "client:"+clientId, 2*time.Hour)

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

	// Set expiration for the room if all clients have left
	var toCheck *string
	if *room.Host == clientId {
		toCheck = room.Guest
	} else if *room.Guest == clientId {
		toCheck = room.Host
	}
	if toCheck == nil {
		redisClient.Expire(ctx, "room:"+roomId, 2*time.Hour)
	} else {
		ttl, err := redisClient.TTL(ctx, "client:"+*toCheck).Result()
		if err != nil {
			log.Printf("Error getting TTL for client %s: %s\n", *toCheck, err)
			return
		}
		// If the other client is still in the room, don't expire the room. Else...
		if ttl == -1 {
			redisClient.Expire(ctx, "room:"+roomId, 2*time.Hour)
		}
	}

	/*switch room.State {
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
	}*/
}

func onMessage(ctx context.Context, messageMessage *MessageMessage) {
	msg := messageMessage.Messages[0]
	log.Printf("%s sent message %v on channel %s\n", msg.ClientId, msg, messageMessage.Channel)

	channel := messageMessage.Channel
	if strings.HasPrefix(channel, "control:") {
		onControlChannelMessage(ctx, messageMessage)
	}
}

func onControlChannelMessage(ctx context.Context, messageMessage *MessageMessage) {
}
