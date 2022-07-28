package shared

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

type RedisCtxKey struct{}
type AblyCtxKey struct{}
type RoomCtxKey struct{}
type ServerChannelCtxKey struct{}
type RedsyncCtxKey struct{}

func WithRoom(ctx context.Context, roomId string) (context.Context, error) {
	rdb := ctx.Value(RedisCtxKey{}).(*redis.Client)
	val, err := rdb.Get(ctx, "room:"+roomId).Result()
	if err != nil {
		if err == redis.Nil {
			return ctx, fmt.Errorf("room %s not exists: %w", roomId, err)
		}
		return ctx, fmt.Errorf("error getting room %s: %w", roomId, err)
	}

	var data Room
	err = json.Unmarshal([]byte(val), &data)
	if err != nil {
		return ctx, fmt.Errorf("error unmarshalling json data: %w. Raw: %s", err, val)
	}
	return context.WithValue(ctx, RoomCtxKey{}, &data), err
}

func SaveRoomToRedis(ctx context.Context, expiration time.Duration) error {
	rdb := ctx.Value(RedisCtxKey{}).(*redis.Client)
	room := ctx.Value(RoomCtxKey{}).(*Room)
	roomJson, err := json.Marshal(room)
	if err != nil {
		return fmt.Errorf("error marshalling room: %w", err)
	}
	rdb.Set(ctx, "room:"+room.Id, roomJson, expiration)
	return nil
}

func MarshallRoom(ctx context.Context) ([]byte, error) {
	room := ctx.Value(RoomCtxKey{}).(*Room)
	roomJson, err := json.Marshal(room)
	if err != nil {
		return nil, fmt.Errorf("error marshalling room: %w", err)
	}
	return roomJson, nil
}

func SaveRoomToRedisAppendPipeline(ctx context.Context, pipe redis.Pipeliner, expiration time.Duration) error {
	room := ctx.Value(RoomCtxKey{}).(*Room)
	roomJson, err := MarshallRoom(ctx)
	if err != nil {
		return fmt.Errorf("error marshalling room: %w", err)
	}
	pipe.Set(ctx, "room:"+room.Id, roomJson, expiration)
	return nil
}

func SaveRoomToRedisWithJsonAppendPipeline(ctx context.Context, roomJson []byte, pipe redis.Pipeliner, expiration time.Duration) {
	room := ctx.Value(RoomCtxKey{}).(*Room)
	pipe.Set(ctx, "room:"+room.Id, roomJson, expiration)
}
