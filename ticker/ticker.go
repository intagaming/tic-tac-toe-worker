package ticker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ably/ably-go/ably"
	"github.com/go-redis/redis/v8"
	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v8"
	"hxann.com/tic-tac-toe-worker/shared"
	"hxann.com/tic-tac-toe-worker/worker"
)

type redsyncCtxKey struct{}
type tickerCtxKey struct{}

type Ticker struct {
	idle          bool
	idleHalfTicks int
	sleepUntil    time.Time
}

const TickTime = 2 * time.Second
const IdleHalfTicksTrigger = 10      // After this amount of half-tick idling, idle mode will be on.
const IdleInterval = 5 * time.Second // In idle mode, we will tick every following interval.
// PushbackTime is the time that the ticker will add into the score of the room while it is processing the room in order
// to prevent the room from being realized by other tickers. This results in the rooms that need ticking the most having
// the lowest scores and being realized before the being-processed task.
const PushbackTime = TickTime * 5

// type tickerIdCtxKey struct{}

// var tickerCounter int = 0

func New(ctx context.Context) func() error {
	// tickerId := tickerCounter
	// tickerCounter++

	return func() error {
		// ctx = context.WithValue(ctx, tickerIdCtxKey{}, tickerId)
		log.Println("Starting ticker")

		rdb := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)
		pool := goredis.NewPool(rdb)
		// Create an instance of redisync to be used to obtain a mutual exclusion lock.
		rs := redsync.New(pool)
		ctx = context.WithValue(ctx, redsyncCtxKey{}, rs)

		ticker := &Ticker{
			idle:          false,
			idleHalfTicks: 0,
			sleepUntil:    time.Now(),
		}
		ctx = context.WithValue(ctx, tickerCtxKey{}, ticker)

		log.Println("Ticker started")

		for {
			select {
			case <-ctx.Done():
				fmt.Println("Breaking out of the loop")
				return nil
			case <-time.After(time.Until(ticker.sleepUntil)):
				tryTick(ctx)
			}
		}
	}
}

type serverChannelCtxKey struct{}

func withServerChannelFromRoomCtx(ctx context.Context) context.Context {
	ablyClient := ctx.Value(shared.AblyCtxKey{}).(*ably.Realtime)
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)
	serverChannel := ablyClient.Channels.Get("server:" + room.Id)
	return context.WithValue(ctx, serverChannelCtxKey{}, serverChannel)
}

func tryTick(ctx context.Context) {
	ticker := ctx.Value(tickerCtxKey{}).(*Ticker)

	rdb := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)
	rs := ctx.Value(redsyncCtxKey{}).(*redsync.Redsync)

	// Keep trying until ticking once, then quit tick()
	for {
		// Find the lowest score task in the queue
		// TODO: change to infinity somehow
		zs, err := rdb.ZRangeArgsWithScores(ctx, redis.ZRangeArgs{
			Key:   "tickingRooms",
			Start: 0,
			Stop:  0,
		}).Result()
		if err != nil {
			log.Println("Error getting lowest score task: ", err)
			return
		}
		if len(zs) == 0 {
			// Sleep half a tick because we're not very busy
			idleHalfTick(ctx)
			return
		}
		candidate := zs[0]
		startTime := time.Now()
		// .Score is in UnixMicro, so we divide by 1e6 to get seconds, but from seconds we multiply by 1e9 to get nanoseconds
		unixSeconds := int64(candidate.Score / 1e6)
		unixNano := int64((candidate.Score/1e6 - float64(unixSeconds)) * 1e9)
		unix := time.Unix(unixSeconds, unixNano)
		if !time.Now().After(unix) {
			// Sleep min(half a tick, time until the task is due)
			if time.Until(unix) < TickTime/2 { // If the task is due soon
				if ticker.idle {
					idleOffWithSleepUntil(ctx, unix)
				} else {
					ticker.sleepUntil = unix
				}
			} else {
				idleHalfTick(ctx)
			}
			return
		}

		// We're in business. If in idle mode, turn it off.
		if ticker.idle {
			idleOff(ctx)
		}
		if ticker.idleHalfTicks > 0 {
			ticker.idleHalfTicks = 0
		}

		// Try to acquire lock on the room
		mutexname := "tick:" + candidate.Member.(string)
		mutex := rs.NewMutex(mutexname)
		if err := mutex.Lock(); err != nil {
			log.Println("Error acquiring lock: ", err)
			continue // Retry
		}

		// After acquiring the lock, check if the task has been processed by another ticker. Happens if the task's time
		// is checked at the same time to be processable by 2 tickers, then both ticker attempts to acquire the lock.
		// The first ticker processes the task, then the second one get the chance, but the task is already processed.
		// Also, if the task is deleted by the worker, the following command will error, and we would skip.
		scoreCheck, err := rdb.ZScore(ctx, "tickingRooms", candidate.Member.(string)).Result()
		if err != nil {
			log.Println("Error getting score: ", err)
			if ok, err := mutex.Unlock(); !ok || err != nil {
				log.Println("Error releasing lock: ", err)
			}
			// Retry immediately
			continue
		}
		if scoreCheck != candidate.Score {
			log.Println("Room " + candidate.Member.(string) + " has already been processed by another ticker")
			if ok, err := mutex.Unlock(); !ok || err != nil {
				log.Println("Error releasing lock: ", err)
			}
			// Retry immediately
			continue
		}

		// Push back the task in order to prevent other tickers from realizing it first.
		rdb.ZAdd(ctx, "tickingRooms", &redis.Z{
			Score:  float64(unix.Add(PushbackTime).UnixMicro()),
			Member: candidate.Member,
		})

		// Getting room
		roomId := candidate.Member.(string)
		tickCtx, err := shared.WithRoom(ctx, roomId)
		if err != nil {
			if errors.Is(err, redis.Nil) {
				// Room does not exist but presents in the ticking set.
				// We need to remove this room from the set.
				rdb.ZRem(ctx, "tickingRooms", roomId)
			}

			// Room exists but we can't get it.
			if ok, err := mutex.Unlock(); !ok || err != nil {
				log.Println("Error releasing lock: ", err)
			}
			// Retry immediately
			continue
		}
		tickCtx = withServerChannelFromRoomCtx(tickCtx)
		// Tick
		tick(tickCtx)

		// Schedule next tick
		correctNextTickTime := unix.Add(TickTime)
		// We just skip late ticks.
		insistedNextTickTime := time.Now().Add(TickTime)
		rdb.ZAdd(ctx, "tickingRooms", &redis.Z{
			Score:  float64(insistedNextTickTime.UnixMicro()),
			Member: candidate.Member,
		})

		if ok, err := mutex.Unlock(); !ok || err != nil {
			log.Println("Error releasing lock: ", err)
			continue
		}

		timeElapsed := time.Since(startTime)
		if time.Now().After(correctNextTickTime) {
			log.Printf("Room %s is late by %v. Don't delay! Tick today.", candidate.Member, time.Until(correctNextTickTime))
			return
		}
		if timeElapsed < TickTime/2 {
			// We only do one ticking every half a tick, so we need to sleep for the remaining time
			ticker.sleepUntil = time.Now().Add(TickTime/2 - timeElapsed)
		}
		return
	}
}

func idleHalfTick(ctx context.Context) {
	ticker := ctx.Value(tickerCtxKey{}).(*Ticker)
	if !ticker.idle && ticker.idleHalfTicks >= IdleHalfTicksTrigger {
		log.Println("Idle mode enabled.")
		ticker.idle = true
		ticker.idleHalfTicks = 0
		ticker.sleepUntil = time.Now().Add(IdleInterval)
		return
	}
	if ticker.idle {
		ticker.sleepUntil = time.Now().Add(IdleInterval)
	} else {
		ticker.idleHalfTicks += 1
		ticker.sleepUntil = time.Now().Add(TickTime / 2)
	}
}

func idleOff(ctx context.Context) {
	ticker := ctx.Value(tickerCtxKey{}).(*Ticker)
	ticker.idle = false
	ticker.idleHalfTicks = 0
	log.Println("Idle mode disabled. Ticking mode enabled.")
}

func idleOffWithSleepUntil(ctx context.Context, sleepUntil time.Time) {
	ticker := ctx.Value(tickerCtxKey{}).(*Ticker)
	idleOff(ctx)
	ticker.sleepUntil = sleepUntil
}

// tick function ticks a room.
func tick(ctx context.Context) {
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)
	serverChannel := ctx.Value(serverChannelCtxKey{}).(*ably.RealtimeChannel)
	rdb := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)

	switch room.State {
	case "waiting":
		var publishMessages []*ably.Message
		var roomUpdated bool = false

		// Remove timeout-ed players
		if room.Guest != nil {
			roomId, err := rdb.Get(ctx, "client:"+room.Guest.Name).Result()
			if err != nil && err != redis.Nil {
				log.Println("Error checking if client exists: ", err)
			} else if err == redis.Nil || roomId != room.Id {
				publishMessages = append(publishMessages, &ably.Message{
					Name: worker.ClientLeft.String(),
					Data: room.Guest,
				})
				room.Guest = nil
				roomUpdated = true
			}
		}
		if room.Host != nil {
			roomId, err := rdb.Get(ctx, "client:"+room.Host.Name).Result()
			if err != nil && err != redis.Nil {
				log.Println("Error checking if client exists: ", err)
			} else if err == redis.Nil || roomId != room.Id {
				publishMessages = append(publishMessages, &ably.Message{
					Name: worker.ClientLeft.String(),
					Data: room.Host,
				})
				room.Host = nil
				roomUpdated = true
			}
		}

		if roomUpdated {
			err := shared.SaveRoomToRedis(ctx, redis.KeepTTL)
			if err != nil {
				panic(err)
			}
		}

		if len(publishMessages) > 0 {
			serverChannel.PublishMultiple(ctx, publishMessages)
		}

	case "playing":
	case "finishing":
		// Check if the room is past gameEndsAt
		if room.Data.GameEndsAt != -1 {
			gameEndsAt := time.Unix(int64(room.Data.GameEndsAt), 0)
			if time.Now().After(gameEndsAt) {
				// Ends the game
				// Reset the room state to waiting
				room.State = "waiting"
				room.Data = shared.TicTacToeData{
					Ticks:      0,
					Board:      []*string{nil, nil, nil, nil, nil, nil, nil, nil, nil},
					Turn:       "host",
					TurnEndsAt: -1,
					GameEndsAt: -1,
				}
				err := shared.SaveRoomToRedis(ctx, redis.KeepTTL)
				if err != nil {
					panic(err)
				}

				// Announce the game ended and room state
				_ = serverChannel.Publish(ctx, worker.GameFinished.String(), "")
				return
			}
			return
		}

		// TODO: Turn timer
	}
}
