package worker

import (
	"context"
	"encoding/json"
	"hxann.com/tic-tac-toe-worker/shared"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/ably/ably-go/ably"
	"github.com/go-redis/redis/v8"
)

const RoomTimeoutTime = 1 * time.Minute

type Announcers int

const (
	HostChange Announcers = iota
	RoomState
	GameStartsNow
	ClientLeft
	PlayerCheckedBox
	Winner
	GameFinishing
	GameFinished
)

func (a Announcers) String() string {
	return [...]string{"HOST_CHANGE", "ROOM_STATE", "GAME_STARTS_NOW", "CLIENT_LEFT", "PLAYER_CHECKED_BOX", "WINNER", "GAME_FINISHING", "GAME_FINISHED"}[a]
}

type CheckedBoxAnnouncement struct {
	HostOrGuest string `json:"hostOrGuest"`
	Box         int    `json:"box"`
}
type WinnerAnnouncement struct {
	Winner     string `json:"winner"`
	GameEndsAt int    `json:"gameEndsAt"`
}

type Actions int

const (
	StartGame Actions = iota
	LeaveRoom
	CheckBox
)

func (a Actions) String() string {
	return [...]string{"START_GAME", "LEAVE_ROOM", "CHECK_BOX"}[a]
}

func roomIdFromControlChannel(channel string) string {
	return strings.Replace(channel, "control:", "", 1)
}

func onEnter(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	log.Printf("%s entered channel %s\n", presence.ClientId, presenceMsg.Channel)

	channel := presenceMsg.Channel
	if strings.HasPrefix(channel, "control:") {
		ctx = shared.WithServerChannelFromChannel(ctx, channel)
		ctx, err := shared.WithRoom(ctx, roomIdFromControlChannel(channel))
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
		ctx = shared.WithServerChannelFromChannel(ctx, channel)
		ctx, err := shared.WithRoom(ctx, roomIdFromControlChannel(channel))
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
	roomId := roomIdFromControlChannel(channel)
	clientId := presence.ClientId
	redisClient := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)
	serverChannel := ctx.Value(shared.ServerChannelCtxKey{}).(*ably.RealtimeChannel)

	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)

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
		_ = serverChannel.Publish(ctx, RoomState.String(), string(roomJson))
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
		_ = serverChannel.Publish(ctx, RoomState.String(), string(roomJson))
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
		_ = serverChannel.Publish(ctx, RoomState.String(), string(roomJson))
	}

	// TODO: do something when the room is full
	// Send the room state
	roomJson, err := json.Marshal(room)
	if err != nil {
		log.Printf("Error marshalling room: %s\n", err)
		return
	}
	_ = serverChannel.Publish(ctx, RoomState.String(), string(roomJson))
}

func expireRoomIfNecessary(ctx context.Context, room *shared.Room, leftClientId string) {
	redisClient := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)

	// Set expiration for the room if all clients have left
	var toCheck *string
	if room.Host != nil && *room.Host == leftClientId {
		toCheck = room.Guest
	} else if room.Guest != nil && *room.Guest == leftClientId {
		toCheck = room.Host
	} else { // If not the host or guest, keep the room alive
		return
	}
	if toCheck == nil { // If there's no one left, expire the room
		redisClient.Expire(ctx, "room:"+room.Id, RoomTimeoutTime)
	} else {
		ttl, err := redisClient.TTL(ctx, "client:"+*toCheck).Result()
		if err != nil {
			log.Printf("Error getting TTL for client %s: %s\n", *toCheck, err)
			return
		}
		// If the other client is still in the room, don't expire the room. Else...
		if ttl != -1 {
			redisClient.Expire(ctx, "room:"+room.Id, RoomTimeoutTime)
		}
	}
}

func onControlChannelLeave(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	// channel := presenceMsg.Channel
	// roomId := roomIdFromControlChannel(channel)
	clientId := presence.ClientId
	redisClient := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)
	// serverChannel := ctx.Value(shared.ServerChannelCtxKey{}).(*ably.RealtimeChannel)

	// The client has some time to join the room again. The due time is before the
	// time the room expires minus 10 seconds. The 10 seconds is the wiggle-room,
	// because if the player joins right at the time that the room is about to
	// expire, the room might have already expired by the time the player
	// establishes connection.
	redisClient.Expire(ctx, "client:"+clientId, RoomTimeoutTime-10*time.Second)

	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)

	expireRoomIfNecessary(ctx, room, clientId)
}

func onMessage(ctx context.Context, messageMessage *MessageMessage) {
	msg := messageMessage.Messages[0]
	log.Printf("%s sent message %v on channel %s\n", msg.ClientId, msg, messageMessage.Channel)

	channel := messageMessage.Channel
	if strings.HasPrefix(channel, "control:") {
		ctx = shared.WithServerChannelFromChannel(ctx, channel)
		ctx, err := shared.WithRoom(ctx, roomIdFromControlChannel(channel))
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
	clientId := msg.ClientId
	redisClient := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)
	serverChannel := ctx.Value(shared.ServerChannelCtxKey{}).(*ably.RealtimeChannel)
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)

	switch msg.Name {
	case StartGame.String():
		if room.State != "waiting" || room.Host == nil || room.Guest == nil || *room.Host != msg.ClientId {
			return
		}

		// Starting the game...
		now := time.Now()
		turnEndsAt := int(now.Add(30 * time.Second).Unix())
		room.State = "playing"
		room.Data.TurnEndsAt = turnEndsAt
		redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.state", "\"playing\"")
		redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.data.turnEndsAt", strconv.Itoa(turnEndsAt))

		// Add the game to the ticker sorted set
		redisClient.ZAdd(ctx, "tickingRooms", &redis.Z{Score: float64(now.UnixMicro()), Member: room.Id})

		roomJson, err := json.Marshal(room)
		if err != nil {
			log.Printf("Error marshalling room: %s\n", err)
			return
		}

		_ = serverChannel.Publish(ctx, GameStartsNow.String(), string(roomJson))
	case LeaveRoom.String():
		// Remove player from room
		clientToRemove := msg.Data
		if room.Host != nil && *room.Host == clientToRemove {
			// If we have a guest, make that guest the new host
			if room.Guest != nil {
				redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.host", "\""+*room.Guest+"\"")
				_ = serverChannel.Publish(ctx, HostChange.String(), *room.Guest)
			} else {
				redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.host", "null")
			}
		} else if room.Guest != nil && *room.Guest == clientToRemove {
			redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.guest", "null")
		}
		_ = serverChannel.Publish(ctx, ClientLeft.String(), clientToRemove)

		// End the game if playing
		if room.State == "playing" {
			redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.state", "\"finishing\"")
			gameEndsAt := int(time.Now().Add(5 * time.Second).Unix())
			_ = serverChannel.Publish(ctx, GameFinishing.String(), strconv.Itoa(gameEndsAt))
		}

		expireRoomIfNecessary(ctx, room, clientToRemove)
	case CheckBox.String():
		boxToCheck, err := strconv.Atoi(msg.Data)
		if err != nil {
			log.Printf("Error parsing box to check: %s\n", err)
			return
		}

		// Check if is host or guest
		var isHost bool
		if room.Host != nil && *room.Host == clientId {
			isHost = true
		} else if room.Guest != nil && *room.Guest == clientId {
			isHost = false
		} else {
			return
		}

		// Check if it's the client's turn
		if room.Host == nil || room.Guest == nil || (clientId == *room.Host && room.Data.Turn != "host") || (clientId == *room.Guest && room.Data.Turn != "guest") {
			return
		}

		// Check if the boxJson is already checked
		boxJson, err := redisClient.Do(ctx, "JSON.GET", "room:"+room.Id, "$.data.board["+strconv.Itoa(boxToCheck)+"]").Result()
		if err != nil {
			log.Printf("Error getting box: %s\n", err)
			return
		}
		var boxes []*string
		err = json.Unmarshal([]byte(boxJson.(string)), &boxes)
		if err != nil {
			log.Printf("Error unmarshalling box: %s\n", err)
			return
		}
		box := boxes[0]
		if box != nil {
			return
		}

		var checkInto string
		if isHost {
			checkInto = "host"
		} else {
			checkInto = "guest"
		}
		redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.data.board["+strconv.Itoa(boxToCheck)+"]", "\""+checkInto+"\"")
		// Update the board to use later
		room.Data.Board[boxToCheck] = &checkInto

		announcement, err := json.Marshal(CheckedBoxAnnouncement{
			HostOrGuest: checkInto,
			Box:         boxToCheck,
		})
		if err != nil {
			log.Printf("Error marshalling announcement: %s\n", err)
			return
		}
		_ = serverChannel.Publish(ctx, PlayerCheckedBox.String(), string(announcement))

		// Check if someone's winning
		winning := checkWin(ctx)
		if winning != nil {
			// Change game state
			redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.state", "\"finishing\"")
			gameEndsAt := int(time.Now().Add(5 * time.Second).Unix())
			redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.data.gameEndsAt", strconv.Itoa(gameEndsAt))

			// Announce winner
			announcement, err := json.Marshal(WinnerAnnouncement{
				Winner:     *winning,
				GameEndsAt: gameEndsAt,
			})
			if err != nil {
				log.Printf("Error marshalling winner announcement: %s\n", err)
				return
			}
			_ = serverChannel.Publish(ctx, Winner.String(), string(announcement))
			return
		}

		var nextTurn string
		if isHost {
			nextTurn = "\"guest\""
		} else {
			nextTurn = "\"host\""
		}
		redisClient.Do(ctx, "JSON.SET", "room:"+room.Id, "$.data.turn", nextTurn)
	}
}

// checkWin returns the winner, either "host" or "guest"
func checkWin(ctx context.Context) *string {
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)
	board := room.Data.Board
	if board[0] != nil && board[1] != nil && board[2] != nil && *board[0] == *board[1] && *board[1] == *board[2] {
		return board[0]
	}
	if board[3] != nil && board[4] != nil && board[5] != nil && *board[3] == *board[4] && *board[4] == *board[5] {
		return board[3]
	}
	if board[6] != nil && board[7] != nil && board[8] != nil && *board[6] == *board[7] && *board[7] == *board[8] {
		return board[6]
	}
	if board[0] != nil && board[3] != nil && board[6] != nil && *board[0] == *board[3] && *board[3] == *board[6] {
		return board[0]
	}
	if board[1] != nil && board[4] != nil && board[7] != nil && *board[1] == *board[4] && *board[4] == *board[7] {
		return board[1]
	}
	if board[2] != nil && board[5] != nil && board[8] != nil && *board[2] == *board[5] && *board[5] == *board[8] {
		return board[2]
	}
	if board[0] != nil && board[4] != nil && board[8] != nil && *board[0] == *board[4] && *board[4] == *board[8] {
		return board[0]
	}
	if board[2] != nil && board[4] != nil && board[6] != nil && *board[2] == *board[4] && *board[4] == *board[6] {
		return board[2]
	}
	return nil
}
