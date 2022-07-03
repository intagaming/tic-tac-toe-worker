package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
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

type Announcers int

const (
	HOST_CHANGE Announcers = iota
	ROOM_STATE
	GAME_STARTS_NOW
)

func (a Announcers) String() string {
	return [...]string{"HOST_CHANGE", "ROOM_STATE", "GAME_STARTS_NOW"}[a]
}

type Actions int

const (
	START_GAME Actions = iota
)

func (a Actions) String() string {
	return [...]string{"START_GAME"}[a]
}

type serverChannelCtxKey struct{}

func withServerChannel(ctx context.Context, channel string) context.Context {
	ablyClient := ctx.Value(ablyCtxKey{}).(*ably.Realtime)
	serverChannel := ablyClient.Channels.Get("server:" + strings.Replace(channel, "control:", "", 1))
	return context.WithValue(ctx, serverChannelCtxKey{}, serverChannel)
}

type roomCtxKey struct{}

func withRoom(ctx context.Context, roomId string) (context.Context, error) {
	redisClient := ctx.Value(redisCtxKey{}).(*redis.Client)
	val, err := redisClient.Do(ctx, "JSON.GET", "room:"+roomId, "$").Result()
	if err != nil {
		if err == redis.Nil {
			return ctx, fmt.Errorf("Room %s not exists", roomId)
		}
		return ctx, fmt.Errorf("error getting room %s: %w", roomId, err)
	}

	var data []Room
	err = json.Unmarshal([]byte(val.(string)), &data)
	if err != nil {
		return ctx, fmt.Errorf("error unmarshalling json data: %w. Raw: %s", err, val.(string))
	}
	return context.WithValue(ctx, roomCtxKey{}, data[0]), err
}

func roomIdFromControlChannel(channel string) string {
	return strings.Split(strings.Replace(channel, "control:", "", 1), ":")[0]
}

func onEnter(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	log.Printf("%s entered channel %s\n", presence.ClientId, presenceMsg.Channel)

	channel := presenceMsg.Channel
	if strings.HasPrefix(channel, "control:") {
		ctx = withServerChannel(ctx, channel)
		ctx, err := withRoom(ctx, roomIdFromControlChannel(channel))
		if err != nil {
			log.Printf("Error getting room: %s", err)
			return
		}
		onControlChannelEnter(ctx, presenceMsg)
	}
}

func onLeave(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	log.Printf("%s left channel %s\n", presence.ClientId, presenceMsg.Channel)

	channel := presenceMsg.Channel
	if strings.HasPrefix(channel, "control:") {
		ctx = withServerChannel(ctx, channel)
		ctx, err := withRoom(ctx, roomIdFromControlChannel(channel))
		if err != nil {
			log.Printf("Error getting room: %s", err)
			return
		}
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

	room := ctx.Value(roomCtxKey{}).(Room)

	if room.Host == nil { // If no host set
		// Set as host
		// TODO: fix race condition when we're setting the host but someone else joins first and became the host?
		redisClient.Do(ctx, "JSON.SET", "room:"+roomId, "$.host", "\""+clientId+"\"")
		// Persists the client's roomId
		redisClient.Set(ctx, "client:"+clientId, roomId, 0)
		// Persists the room
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
	} else if *room.Host != clientId && room.Guest == nil { // If not the host and no guest set
		// Set as guest
		redisClient.Do(ctx, "JSON.SET", "room:"+roomId, "$.guest", "\""+clientId+"\"")
		// Persists the client's roomId
		redisClient.Set(ctx, "client:"+clientId, roomId, 0)
		// Persists the room
		redisClient.Persist(ctx, "room:"+roomId)
		room.Guest = &clientId

		// Send the room state
		roomJson, err := json.Marshal(room)
		if err != nil {
			log.Printf("Error marshalling room: %s\n", err)
			return
		}
		serverChannel.Publish(ctx, ROOM_STATE.String(), string(roomJson))
		return
	} else if *room.Host == clientId || *room.Guest == clientId { // If re-joining
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

	// TODO: do something when the room is full
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

	// The client has 10 minutes to join the room again
	redisClient.Expire(ctx, "client:"+clientId, 10*time.Minute)

	room := ctx.Value(roomCtxKey{}).(Room)

	// Set expiration for the room if all clients have left
	var toCheck *string
	if room.Host != nil && *room.Host == clientId {
		toCheck = room.Guest
	} else if room.Guest != nil && *room.Guest == clientId {
		toCheck = room.Host
	}
	if toCheck == nil {
		redisClient.Expire(ctx, "room:"+roomId, 10*time.Minute)
	} else {
		ttl, err := redisClient.TTL(ctx, "client:"+*toCheck).Result()
		if err != nil {
			log.Printf("Error getting TTL for client %s: %s\n", *toCheck, err)
			return
		}
		// If the other client is still in the room, don't expire the room. Else...
		if ttl != -1 {
			redisClient.Expire(ctx, "room:"+roomId, 10*time.Minute)
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
		ctx = withServerChannel(ctx, channel)
		ctx, err := withRoom(ctx, roomIdFromControlChannel(channel))
		if err != nil {
			log.Printf("Error getting room: %s", err)
			return
		}
		onControlChannelMessage(ctx, messageMessage)
	}
}

func onControlChannelMessage(ctx context.Context, messageMessage *MessageMessage) {
	msg := messageMessage.Messages[0]
	// channel := messageMessage.Channel
	// roomId := strings.Split(strings.Replace(channel, "control:", "", 1), ":")[0]
	// clientId := msg.ClientId
	redisClient := ctx.Value(redisCtxKey{}).(*redis.Client)
	serverChannel := ctx.Value(serverChannelCtxKey{}).(*ably.RealtimeChannel)
	room := ctx.Value(roomCtxKey{}).(Room)

	switch msg.Name {
	case START_GAME.String():
		if room.State != "waiting" || room.Host == nil || room.Guest == nil || *room.Host != msg.ClientId {
			return
		}

		// Starting the game...
		turnEndsAt := int(time.Now().Add(30 * time.Second).Unix())
		room.State = "playing"
		room.Data.TurnEndsAt = turnEndsAt
		redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.state", "\"playing\"")
		redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.data.turnEndsAt", strconv.Itoa(turnEndsAt))

		roomJson, err := json.Marshal(room)
		if err != nil {
			log.Printf("Error marshalling room: %s\n", err)
			return
		}

		serverChannel.Publish(ctx, GAME_STARTS_NOW.String(), string(roomJson))
	}
}
