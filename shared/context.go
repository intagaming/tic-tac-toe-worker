package shared

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-redis/redis/v8"
)

type RedisCtxKey struct{}
type AblyCtxKey struct{}

type RoomCtxKey struct{}

func WithRoom(ctx context.Context, roomId string) (context.Context, error) {
	redisClient := ctx.Value(RedisCtxKey{}).(*redis.Client)
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
	return context.WithValue(ctx, RoomCtxKey{}, &data[0]), err
}
