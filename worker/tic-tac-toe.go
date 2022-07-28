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
	"github.com/go-redsync/redsync/v4"
)

const RoomTimeoutTime = 1 * time.Minute

type Announcers int

const (
	HostChange Announcers = iota
	RoomState
	GameStartsNow
	ClientLeft
	ClientDisconnected
	ClientReconnected
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
		"CLIENT_RECONNECTED",
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

func withServerChannelFromChannel(ctx context.Context, channel string) context.Context {
	ablyClient := ctx.Value(shared.AblyCtxKey{}).(*ably.Realtime)
	serverChannel := ablyClient.Channels.Get("server:" + strings.Replace(channel, "control:", "", 1))
	return context.WithValue(ctx, shared.ServerChannelCtxKey{}, serverChannel)
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
	serverChannel := ctx.Value(shared.ServerChannelCtxKey{}).(*ably.RealtimeChannel)
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)

	// Lock room
	rs := ctx.Value(shared.RedsyncCtxKey{}).(*redsync.Redsync)
	mutexName := shared.RoomLockName(room.Id)
	mutex := rs.NewMutex(mutexName)
	if err := mutex.Lock(); err != nil {
		log.Println("Error acquiring lock: ", err)
		// We couldn't acquire lock. We decide to drop the request.
		return
	}
	// Defer release lock
	defer func() {
		if ok, err := mutex.Unlock(); !ok || err != nil {
			log.Println("Error releasing lock: ", err)
		}
	}()

	if room.Host == nil { // If no host set
		// Set as host
		// TODO: fix race condition when we're setting the host but someone else joins first and became the host?
		room.Host = &shared.Player{
			Name:      clientId,
			Connected: true,
		}

		roomJson, err := shared.MarshallRoom(ctx)
		if err != nil {
			log.Println(err)
			return
		}

		pipe := rdb.Pipeline()

		// Save, and persist the room
		err = shared.SaveRoomToRedisWithJsonInPipeline(ctx, roomJson, pipe, 0)
		if err != nil {
			panic(err)
		}
		// Persists the client's roomId
		pipe.Set(ctx, "client:"+clientId, roomId, 0)

		if _, err := pipe.Exec(ctx); err != nil {
			log.Println("Error executing pipeline: ", err)
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
		roomJson, err := shared.MarshallRoom(ctx)
		if err != nil {
			log.Println(err)
			return
		}

		pipe := rdb.Pipeline()

		// Save, and persist the room
		err = shared.SaveRoomToRedisWithJsonInPipeline(ctx, roomJson, pipe, 0)
		if err != nil {
			panic(err)
		}
		// Persists the client's roomId
		pipe.Set(ctx, "client:"+clientId, roomId, 0)

		if _, err := pipe.Exec(ctx); err != nil {
			log.Println("Error executing pipeline: ", err)
			return
		}

		_ = serverChannel.Publish(ctx, RoomState.String(), string(roomJson))
		return
	} else if room.Host.Name == clientId || room.Guest.Name == clientId { // If re-joining

		pipe := rdb.Pipeline()
		// Persists the client's roomId
		pipe.Set(ctx, "client:"+clientId, roomId, 0)
		// Persists the room
		pipe.Persist(ctx, "room:"+roomId)

		if room.Host != nil && room.Host.Name == clientId {
			room.Host.Connected = true
		} else if room.Guest != nil && room.Guest.Name == clientId {
			room.Guest.Connected = true
		}
		roomJson, err := shared.MarshallRoom(ctx)
		if err != nil {
			log.Println(err)
			return
		}
		shared.SaveRoomToRedisWithJsonInPipeline(ctx, roomJson, pipe, redis.KeepTTL)

		if _, err := pipe.Exec(ctx); err != nil {
			log.Println("Error executing pipeline: ", err)
			return
		}

		serverChannel.PublishMultiple(ctx, []*ably.Message{
			{
				Name: RoomState.String(),
				Data: string(roomJson),
			},
			{
				Name: ClientReconnected.String(),
				Data: clientId,
			},
		})
		return
	}
}

func onControlChannelLeave(ctx context.Context, presenceMsg *PresenceMessage) {
	presence := presenceMsg.Presence[0]
	clientId := presence.ClientId
	rdb := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)
	serverChannel := ctx.Value(shared.ServerChannelCtxKey{}).(*ably.RealtimeChannel)
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)

	// Lock room
	rs := ctx.Value(shared.RedsyncCtxKey{}).(*redsync.Redsync)
	mutexName := shared.RoomLockName(room.Id)
	mutex := rs.NewMutex(mutexName)
	if err := mutex.Lock(); err != nil {
		log.Println("Error acquiring lock: ", err)
		// We couldn't acquire lock. We decide to drop the request.
		return
	}
	// Defer release lock
	defer func() {
		if ok, err := mutex.Unlock(); !ok || err != nil {
			log.Println("Error releasing lock: ", err)
		}
	}()

	pipe := rdb.Pipeline()

	// The client has some time to join the room again. The due time is before the
	// time the room expires minus 10 seconds. The 10 seconds is the wiggle-room,
	// because if the player joins right at the time that the room is about to
	// expire, the room might have already expired by the time the player
	// establishes connection.
	pipe.Expire(ctx, "client:"+clientId, RoomTimeoutTime-10*time.Second)
	if room.Host != nil && room.Host.Name == clientId {
		room.Host.Connected = false
	} else if room.Guest != nil && room.Guest.Name == clientId {
		room.Guest.Connected = false
	}
	shared.SaveRoomToRedisInPipeline(ctx, pipe, redis.KeepTTL)

	if _, err := pipe.Exec(ctx); err != nil {
		log.Println("Error executing pipeline: ", err)
		return
	}

	serverChannel.Publish(ctx, ClientDisconnected.String(), clientId)
}

func onMessage(ctx context.Context, messageMessage *MessageMessage) {
	// msg := messageMessage.Messages[0]
	// log.Printf("%s sent message %v on channel %s\n", msg.ClientId, msg, messageMessage.Channel)

	channel := messageMessage.Channel
	if strings.HasPrefix(channel, "control:") {
		ctx = withServerChannelFromChannel(ctx, channel)
		ctx, err := shared.WithRoom(ctx, roomIdFromControlChannel(channel))
		if err != nil {
			log.Printf("Error getting room: %s\n", err)
			return
		}
		onControlChannelMessage(ctx, messageMessage)
	}
}

func RemovePlayerFromRoom(ctx context.Context, clientToRemove string) {
	serverChannel := ctx.Value(shared.ServerChannelCtxKey{}).(*ably.RealtimeChannel)

	rdb := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)
	pipe := rdb.Pipeline()

	publishMessages := RemovePlayerFromRoomInPipeline(ctx, pipe, clientToRemove)

	if _, err := pipe.Exec(ctx); err != nil {
		log.Println("Error executing pipeline: ", err)
		return
	}

	serverChannel.PublishMultiple(ctx, publishMessages)
}

// RemovePlayerFromRoomInPipeline removes a player from a room, but all Redis
// calls are appended to a pipeline. It returns the messages that must be
// announced on the serverChannel after the pipeline is executed to announce
// that the player is removed.
func RemovePlayerFromRoomInPipeline(ctx context.Context, pipe redis.Pipeliner, clientToRemove string) []*ably.Message {
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)

	var publishMessages []*ably.Message
	var roomChanged bool = false

	if room.Host != nil && room.Host.Name == clientToRemove {
		// If we have a guest, make that guest the new host
		if room.Guest != nil {
			publishMessages = append(publishMessages, &ably.Message{
				Name: HostChange.String(),
				Data: room.Guest.Name,
			})

			room.Host = room.Guest
			room.Guest = nil
			roomChanged = true
		} else {
			room.Host = nil
			roomChanged = true
		}
	} else if room.Guest != nil && room.Guest.Name == clientToRemove {
		room.Guest = nil
		roomChanged = true
	}
	publishMessages = append(publishMessages, &ably.Message{
		Name: ClientLeft.String(),
		Data: clientToRemove,
	})

	// End the game if playing
	if room.State == "playing" {
		room.State = "finishing"
		gameEndsAt := int(time.Now().Add(5 * time.Second).Unix())
		room.Data.GameEndsAt = gameEndsAt
		roomChanged = true
		publishMessages = append(publishMessages, &ably.Message{
			Name: GameFinishing.String(),
			Data: strconv.Itoa(gameEndsAt),
		})
	}

	if roomChanged {
		err := shared.SaveRoomToRedisInPipeline(ctx, pipe, redis.KeepTTL)
		if err != nil {
			panic(err)
		}
	}

	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("Error running remove player pipeline: %s", err)
		return nil
	}

	return publishMessages
}

func onControlChannelMessage(ctx context.Context, messageMessage *MessageMessage) {
	msg := messageMessage.Messages[0]
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)
	clientId := msg.ClientId
	serverChannel := ctx.Value(shared.ServerChannelCtxKey{}).(*ably.RealtimeChannel)

	// Lock room
	rs := ctx.Value(shared.RedsyncCtxKey{}).(*redsync.Redsync)
	mutexName := shared.RoomLockName(room.Id)
	mutex := rs.NewMutex(mutexName)
	if err := mutex.Lock(); err != nil {
		log.Println("Error acquiring lock: ", err)
		// We couldn't acquire lock. We decide to drop the request.
		return
	}
	// Defer release lock
	defer func() {
		if ok, err := mutex.Unlock(); !ok || err != nil {
			log.Println("Error releasing lock: ", err)
		}
	}()

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

		roomJson, err := shared.MarshallRoom(ctx)
		if err != nil {
			log.Println(err)
			return
		}
		_ = serverChannel.Publish(ctx, GameStartsNow.String(), string(roomJson))
	case LeaveRoom.String():
		// Remove player from room
		clientToRemove := msg.Data
		RemovePlayerFromRoom(ctx, clientToRemove)
	case CheckBox.String():
		boxToCheck, err := strconv.Atoi(msg.Data)
		if err != nil {
			log.Printf("Error parsing box to check: %s\n", err)
			return
		}

		var publishMessages []*ably.Message

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

		announcement, err := json.Marshal(CheckedBoxAnnouncement{
			HostOrGuest: checkInto,
			Box:         boxToCheck,
		})
		if err != nil {
			log.Printf("Error marshalling announcement: %s\n", err)
			return
		}
		publishMessages = append(publishMessages, &ably.Message{
			Name: PlayerCheckedBox.String(),
			Data: string(announcement),
		})

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
			publishMessages = append(publishMessages, &ably.Message{
				Name: GameFinishing.String(),
				Data: strconv.Itoa(gameEndsAt),
			})
			publishMessages = append(publishMessages, &ably.Message{
				Name: GameResultAnnounce.String(),
				Data: string(announcement),
			})
		} else {
			var nextTurn string
			if isHost {
				nextTurn = "guest"
			} else {
				nextTurn = "host"
			}
			room.Data.Turn = nextTurn
		}

		err = shared.SaveRoomToRedis(ctx, redis.KeepTTL)
		if err != nil {
			panic(err)
		}

		serverChannel.PublishMultiple(ctx, publishMessages)
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
