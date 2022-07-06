package ticker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ably/ably-go/ably"
	"github.com/go-redis/redis/v8"
	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v8"
	"hxann.com/tic-tac-toe-worker/shared"
	"hxann.com/tic-tac-toe-worker/worker"
	"log"
	"os"
	"time"
)

type redisCtxKey struct{}
type redsyncCtxKey struct{}
type ablyCtxKey struct{}
type tickerCtxKey struct{}

type Ticker struct {
	idle          bool
	idleHalfTicks int
	sleepUntil    time.Time
}

const TickTime = 2 * time.Second
const IdleHalfTicksTrigger = 10       // After this amount of half-tick idling, idle mode will be on.
const IdleInterval = 10 * time.Second // In idle mode, we will tick every following interval.

func New(ctx context.Context) func() error {
	return func() error {
		ablyApiKey := os.Getenv("ABLY_API_KEY")

		// Redis
		opt, err := redis.ParseURL(os.Getenv("REDIS_URL"))
		if err != nil {
			panic(err)
		}
		rdb := redis.NewClient(opt)
		ctx = context.WithValue(ctx, redisCtxKey{}, rdb)

		pool := goredis.NewPool(rdb)
		// Create an instance of redisync to be used to obtain a mutual exclusion lock.
		rs := redsync.New(pool)
		ctx = context.WithValue(ctx, redsyncCtxKey{}, rs)

		// Ably
		ablyClient, err := ably.NewRealtime(ably.WithKey(ablyApiKey))
		if err != nil {
			panic(err)
		}
		ctx = context.WithValue(ctx, ablyCtxKey{}, ablyClient)

		ticker := &Ticker{
			idle:          false,
			idleHalfTicks: 0,
			sleepUntil:    time.Now(),
		}
		ctx = context.WithValue(ctx, tickerCtxKey{}, ticker)

		for {
			select {
			case <-ctx.Done():
				fmt.Println("Breaking out of the loop")
				return nil
			case <-time.After(ticker.sleepUntil.Sub(time.Now())):
				tryTick(ctx)
			}
		}
	}
}

func tryTick(ctx context.Context) {
	ticker := ctx.Value(tickerCtxKey{}).(*Ticker)

	log.Println("--- Trying to find something to tick")

	rdb := ctx.Value(redisCtxKey{}).(*redis.Client)
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
			log.Println("No tasks in the queue")
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
		if err != nil {
			panic(err)
		}
		if !time.Now().After(unix) {
			log.Println("Not yet need to tick, relaxing half a tick or until the task is due")
			// Sleep min(half a tick, time until the task is due)
			if unix.Sub(time.Now()) < TickTime/2 { // If the task is due soon
				if ticker.idle {
					idleOffWithSleepUntil(ctx, time.Now().Add(unix.Sub(time.Now())))
				} else {
					ticker.sleepUntil = time.Now().Add(unix.Sub(time.Now()))
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

		log.Println("Acquiring lock for room: ", candidate.Member)
		// Try to acquire lock on the room
		mutexname := "tick:" + candidate.Member.(string)
		mutex := rs.NewMutex(mutexname)
		if err := mutex.Lock(); err != nil {
			panic(err)
		}

		// After acquiring the lock, check if the task has been processed by another ticker. Happens if the task's time
		// is checked at the same time to be processable by 2 tickers, then both ticker attempts to acquire the lock.
		// The first ticker processes the task, then the second one get the chance, but the task is already processed.
		// Also, if the task is deleted by the worker, the following command will error, and we would skip.
		scoreCheck, err := rdb.ZScore(ctx, "tickingRooms", candidate.Member.(string)).Result()
		if err != nil {
			panic(err)
		}
		if scoreCheck != candidate.Score {
			log.Println("Room " + candidate.Member.(string) + " has already been processed by another ticker")
			// Retry immediately
			continue
		}

		roomId := candidate.Member.(string)
		tickCtx, err := shared.WithRoom(ctx, roomId)
		if err != nil {
			panic(err)
		}
		tickCtx = shared.WithServerChannelFromRoomCtx(tickCtx)
		willTickMore := tick(tickCtx, roomId)

		var nextTickTime time.Time
		if !willTickMore {
			// Remove the room from the tickingRooms list
			rdb.ZRem(ctx, "tickingRooms", roomId)
		} else {
			// Schedule next tick
			nextTickTime = unix.Add(TickTime)
			rdb.ZAdd(ctx, "tickingRooms", &redis.Z{
				Score:  float64(nextTickTime.UnixMicro()),
				Member: candidate.Member,
			})
		}

		log.Println("Releasing lock")
		if ok, err := mutex.Unlock(); !ok || err != nil {
			panic("unlock failed")
		}

		timeElapsed := time.Now().Sub(startTime)
		log.Println("Time elapsed for room: ", candidate.Member, ": ", timeElapsed)
		if willTickMore && time.Now().After(nextTickTime) {
			log.Println("Room ", candidate.Member, " is late. Don't delay! Tick today.")
			return
		}
		if timeElapsed < TickTime/2 {
			// We only do one ticking every half a tick, so we need to sleep for the remaining time
			log.Println("Doing task for shorter than half a tick, sleeping for ", TickTime/2-timeElapsed)
			ticker.sleepUntil = time.Now().Add(TickTime/2 - timeElapsed)
		}
		return
	}
}

func idleHalfTick(ctx context.Context) {
	ticker := ctx.Value(tickerCtxKey{}).(*Ticker)
	if ticker.idle == false && ticker.idleHalfTicks >= IdleHalfTicksTrigger {
		log.Println("Idle mode enabled.")
		ticker.idle = true
		ticker.idleHalfTicks = 0
		ticker.sleepUntil = time.Now().Add(IdleInterval)
		return
	}
	if ticker.idle == true {
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

// tick function ticks a room and returns whether the room will tick again.
func tick(ctx context.Context, roomId string) bool {
	room := ctx.Value(shared.RoomCtxKey{}).(*shared.Room)
	rdb := ctx.Value(shared.RedisCtxKey{}).(*redis.Client)
	serverChannel := ctx.Value(shared.ServerChannelCtxKey{}).(*ably.RealtimeChannel)

	log.Println("Ticking room: " + roomId)
	log.Println("Simulating 0.2tick tick time for room: ", roomId)
	time.Sleep(TickTime / 10 * 2)

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
			roomJson, err := json.Marshal(room)
			if err != nil {
				panic(err)
			}
			rdb.Do(ctx, "JSON.SET", "room:"+roomId, "$", string(roomJson))

			// Announce the game ended and room state
			_ = serverChannel.Publish(ctx, worker.GameFinished.String(), "")
			return false
		}
		return true
	}

	// TODO: Turn timer

	return true
}
