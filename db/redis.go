package db

import (
	"context"
	"fmt"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/redis/go-redis/v9"
)

var Redis *redis.Client

func ConnectRedis() error {
	Redis = redis.NewClient(&redis.Options{
		Addr:     config.C.RedisAddr,
		Password: config.C.RedisPassword,
	})
	if err := Redis.Ping(context.Background()).Err(); err != nil {
		return fmt.Errorf("redis connect: %w", err)
	}
	return nil
}
