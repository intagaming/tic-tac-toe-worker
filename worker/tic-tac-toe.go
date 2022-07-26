package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"hxann.com/tic-tac-toe-worker/shared"

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
	ClientDisconnected
	PlayerCheckedBox
	GameResultAnnounce
	GameFinishing
	GameFinished
)

func (a Announcers) String() string {
	return [...]string{
		"HOST_CHANGE",
		"ROOM_STATE",
		"GAME_STARTS_NOW",
		"CLIENT_LEFT",
		"CLIENT_DISCONNECTED",
		"PLAYER_CHECKED_BOX",
		"GAME_RESULT",
		"GAME_FINISHING",
		"GAME_FINISHED",
	}[a]
}

type CheckedBoxAnnouncement struct {
	HostOrGuest string `json:"hostOrGuest"`
	Box         int    `json:"box"`
}
type GameResultAnnouncement struct {
	Winner     *string `json:"winner"`
	GameEndsAt int     `json:"gameEndsAt"`
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

type serverChannelCtxKey struct{}

func withServerChannelFromChannel(ctx context.Context, channel string) context.Context {
	ablyClient := ctx.Value(shared.AblyCtxKey{}).(*ably.Realtime)
	serverChannel := ablyClient.Channels.Get("server:" + strings.Replace(channel, "control:", "", 1))
	return context.WithValue(ctx, serverChannelCtxKey{}, serverChannel)
}

func onEnter(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	log.Printf("%s entered channel %s\n", presence.ClientId, presenceMsg.Channel)

	channel := presenceMsg.Channel
	if strings.HasPrefix(channel, "control:") {
		ctx = withServerChannelFromChannel(ctx, channel)
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
		ctx = withServerChannelFromChannel(ctx, channel)
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
	rdb := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)
	serverChannel := ctx.Value(serverChannelCtxKey{}).(*ably.RealtimeChannel)

	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)

	if room.Host == nil { // If no host set
		// Set as host
		// TODO: fix race condition when we're setting the host but someone else joins first and became the host?
		room.Host = &shared.Player{
			Name:      clientId,
			Connected: true,
		}
		// Save, and persist the room
		err := shared.SaveRoomToRedis(ctx, 0)
		if err != nil {
			panic(err)
		}
		// Persists the client's roomId
		rdb.Set(ctx, "client:"+clientId, roomId, 0)

		// Send the room state
		roomJson, err := json.Marshal(room)
		if err != nil {
			log.Printf("Error marshalling room: %s\n", err)
			return
		}
		_ = serverChannel.Publish(ctx, RoomState.String(), string(roomJson))
		return
	} else if room.Host.Name != clientId && room.Guest == nil { // If not the host and no guest set
		// Set as guest
		room.Guest = &shared.Player{
			Name:      clientId,
			Connected: true,
		}
		// Save, and persist the room
		err := shared.SaveRoomToRedis(ctx, 0)
		if err != nil {
			panic(err)
		}
		// Persists the client's roomId
		rdb.Set(ctx, "client:"+clientId, roomId, 0)

		// Send the room state
		roomJson, err := json.Marshal(room)
		if err != nil {
			log.Printf("Error marshalling room: %s\n", err)
			return
		}
		_ = serverChannel.Publish(ctx, RoomState.String(), string(roomJson))
		return
	} else if room.Host.Name == clientId || room.Guest.Name == clientId { // If re-joining
		// Persists the client's roomId
		rdb.Set(ctx, "client:"+clientId, roomId, 0)
		// Persists the room
		rdb.Persist(ctx, "room:"+roomId)

		if room.Host != nil && room.Host.Name == clientId {
			room.Host.Connected = true
			shared.SaveRoomToRedis(ctx, redis.KeepTTL)
		} else if room.Guest != nil && room.Guest.Name == clientId {
			room.Guest.Connected = true
			shared.SaveRoomToRedis(ctx, redis.KeepTTL)
		}

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
	rdb := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)

	// Set expiration for the room if all clients have left
	// If not the host or guest, keep the room alive
	if !((room.Host != nil && leftClientId == room.Host.Name) || (room.Guest != nil && leftClientId == room.Guest.Name)) {
		return
	}
	var toCheck *string
	if room.Host != nil && room.Host.Name == leftClientId && room.Guest != nil {
		toCheck = &room.Guest.Name
	} else if room.Guest != nil && room.Guest.Name == leftClientId && room.Host != nil {
		toCheck = &room.Host.Name
	}
	if toCheck == nil { // If there's no one left, expire the room
		rdb.Expire(ctx, "room:"+room.Id, RoomTimeoutTime)
	} else {
		pipe := rdb.Pipeline()
		ttl := pipe.TTL(ctx, "client:"+*toCheck)
		clientRoomId := pipe.Get(ctx, "client:"+*toCheck)

		_, err := pipe.Exec(ctx)
		if err != nil && err != redis.Nil {
			log.Printf("Error getting TTL and clientRoomId for client %s: %s\n", *toCheck, err)
			return
		}
		// Even when we didn't find the client, we still expire the room anyway, because the client is nowhere to be found.

		// If the other client disappeared, or is not in the room, or the room they're in is not the room in question, then expire the room.
		if err == redis.Nil || ttl.Val() != -1 || clientRoomId.Val() != room.Id {
			rdb.Expire(ctx, "room:"+room.Id, RoomTimeoutTime)
		}
	}
}

func onControlChannelLeave(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	clientId := presence.ClientId
	rdb := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)
	serverChannel := ctx.Value(serverChannelCtxKey{}).(*ably.RealtimeChannel)
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)

	// The client has some time to join the room again. The due time is before the
	// time the room expires minus 10 seconds. The 10 seconds is the wiggle-room,
	// because if the player joins right at the time that the room is about to
	// expire, the room might have already expired by the time the player
	// establishes connection.
	rdb.Expire(ctx, "client:"+clientId, RoomTimeoutTime-10*time.Second)
	if room.Host != nil && room.Host.Name == clientId {
		room.Host.Connected = false
		shared.SaveRoomToRedis(ctx, redis.KeepTTL)
	} else if room.Guest != nil && room.Guest.Name == clientId {
		room.Guest.Connected = false
		shared.SaveRoomToRedis(ctx, redis.KeepTTL)
	}
	serverChannel.Publish(ctx, ClientDisconnected.String(), clientId)

	expireRoomIfNecessary(ctx, room, clientId)
}

func onMessage(ctx context.Context, messageMessage *MessageMessage) {
	msg := messageMessage.Messages[0]
	log.Printf("%s sent message %v on channel %s\n", msg.ClientId, msg, messageMessage.Channel)

	channel := messageMessage.Channel
	if strings.HasPrefix(channel, "control:") {
		ctx = withServerChannelFromChannel(ctx, channel)
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
	clientId := msg.ClientId
	serverChannel := ctx.Value(serverChannelCtxKey{}).(*ably.RealtimeChannel)
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)

	switch msg.Name {
	case StartGame.String():
		if room.State != "waiting" || room.Host == nil || room.Guest == nil || room.Host.Name != msg.ClientId {
			return
		}

		// Starting the game...
		now := time.Now()
		turnEndsAt := int(now.Add(30 * time.Second).Unix())
		room.State = "playing"
		room.Data.TurnEndsAt = turnEndsAt
		err := shared.SaveRoomToRedis(ctx, redis.KeepTTL)
		if err != nil {
			panic(err)
		}

		roomJson, err := json.Marshal(room)
		if err != nil {
			log.Printf("Error marshalling room: %s\n", err)
			return
		}

		_ = serverChannel.Publish(ctx, GameStartsNow.String(), string(roomJson))
	case LeaveRoom.String():
		// Remove player from room
		clientToRemove := msg.Data
		if room.Host != nil && room.Host.Name == clientToRemove {
			// If we have a guest, make that guest the new host
			if room.Guest != nil {
				_ = serverChannel.Publish(ctx, HostChange.String(), *room.Guest)

				room.Host = room.Guest
				room.Guest = nil
				err := shared.SaveRoomToRedis(ctx, redis.KeepTTL)
				if err != nil {
					panic(err)
				}
			} else {
				room.Host = nil
				err := shared.SaveRoomToRedis(ctx, redis.KeepTTL)
				if err != nil {
					panic(err)
				}
			}
		} else if room.Guest != nil && room.Guest.Name == clientToRemove {
			room.Guest = nil
			err := shared.SaveRoomToRedis(ctx, redis.KeepTTL)
			if err != nil {
				panic(err)
			}
		}
		_ = serverChannel.Publish(ctx, ClientLeft.String(), clientToRemove)

		// End the game if playing
		if room.State == "playing" {
			room.State = "finishing"
			gameEndsAt := int(time.Now().Add(5 * time.Second).Unix())
			room.Data.GameEndsAt = gameEndsAt
			err := shared.SaveRoomToRedis(ctx, redis.KeepTTL)
			if err != nil {
				panic(err)
			}
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
		if room.Host != nil && room.Host.Name == clientId {
			isHost = true
		} else if room.Guest != nil && room.Guest.Name == clientId {
			isHost = false
		} else {
			return
		}

		// Check if it's the client's turn
		if room.Host == nil || room.Guest == nil || (clientId == room.Host.Name && room.Data.Turn != "host") || (clientId == room.Guest.Name && room.Data.Turn != "guest") {
			return
		}

		// Check if the boxJson is already checked
		box := room.Data.Board[boxToCheck]
		if box != nil {
			return
		}

		var checkInto string
		if isHost {
			checkInto = "host"
		} else {
			checkInto = "guest"
		}
		// Update the board
		room.Data.Board[boxToCheck] = &checkInto
		err = shared.SaveRoomToRedis(ctx, redis.KeepTTL)
		if err != nil {
			panic(err)
		}

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
		result, err := gameResult(ctx)
		if err != nil {
			log.Printf("Error getting game result: %s\n", err)
			return
		}
		if result != Undecided {
			// Change game state
			room.State = "finishing"
			gameEndsAt := int(time.Now().Add(5 * time.Second).Unix())
			room.Data.GameEndsAt = gameEndsAt
			err := shared.SaveRoomToRedis(ctx, redis.KeepTTL)
			if err != nil {
				panic(err)
			}

			// Announce game result
			var winnerClientId *string // If the game is draw, the winnerClientId will be nil
			if result == HostWin {
				winnerClientId = &room.Host.Name
			} else if result == GuestWin {
				winnerClientId = &room.Guest.Name
			}
			announcement, err := json.Marshal(GameResultAnnouncement{
				Winner:     winnerClientId,
				GameEndsAt: gameEndsAt,
			})
			if err != nil {
				log.Printf("Error marshalling winner announcement: %s\n", err)
				return
			}
			_ = serverChannel.Publish(ctx, GameFinishing.String(), strconv.Itoa(gameEndsAt))
			_ = serverChannel.Publish(ctx, GameResultAnnounce.String(), string(announcement))
			return
		}

		var nextTurn string
		if isHost {
			nextTurn = "guest"
		} else {
			nextTurn = "host"
		}
		room.Data.Turn = nextTurn
		err = shared.SaveRoomToRedis(ctx, redis.KeepTTL)
		if err != nil {
			panic(err)
		}
	}
}

type GameResult int

const (
	Undecided GameResult = iota
	HostWin
	GuestWin
	Draw
)

func gameResultBasedOnString(hostOrGuest string) (GameResult, error) {
	if hostOrGuest == "host" {
		return HostWin, nil
	}
	if hostOrGuest == "guest" {
		return GuestWin, nil
	}
	return Undecided, fmt.Errorf("hostOrGuest is not 'host' or 'guest'. Value: %s", hostOrGuest)
}

// gameResult returns the game result.
func gameResult(ctx context.Context) (GameResult, error) {
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)
	board := room.Data.Board

	if board[0] != nil && board[1] != nil && board[2] != nil && *board[0] == *board[1] && *board[1] == *board[2] {
		return gameResultBasedOnString(*board[0])
	}
	if board[3] != nil && board[4] != nil && board[5] != nil && *board[3] == *board[4] && *board[4] == *board[5] {
		return gameResultBasedOnString(*board[3])
	}
	if board[6] != nil && board[7] != nil && board[8] != nil && *board[6] == *board[7] && *board[7] == *board[8] {
		return gameResultBasedOnString(*board[6])
	}
	if board[0] != nil && board[3] != nil && board[6] != nil && *board[0] == *board[3] && *board[3] == *board[6] {
		return gameResultBasedOnString(*board[0])
	}
	if board[1] != nil && board[4] != nil && board[7] != nil && *board[1] == *board[4] && *board[4] == *board[7] {
		return gameResultBasedOnString(*board[1])
	}
	if board[2] != nil && board[5] != nil && board[8] != nil && *board[2] == *board[5] && *board[5] == *board[8] {
		return gameResultBasedOnString(*board[2])
	}
	if board[0] != nil && board[4] != nil && board[8] != nil && *board[0] == *board[4] && *board[4] == *board[8] {
		return gameResultBasedOnString(*board[0])
	}
	if board[2] != nil && board[4] != nil && board[6] != nil && *board[2] == *board[4] && *board[4] == *board[6] {
		return gameResultBasedOnString(*board[2])
	}

	// Check if draw
	if board[0] != nil && board[1] != nil && board[2] != nil && board[3] != nil && board[4] != nil && board[5] != nil && board[6] != nil && board[7] != nil && board[8] != nil {
		return Draw, nil
	}

	return Undecided, nil
}
