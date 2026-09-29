package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const tokenBucketLua = `
local now = redis.call('TIME')
local now_ms = now[1] * 1000 + math.floor(now[2] / 1000)
local capacity = tonumber(ARGV[1])
local refill_per_ms = tonumber(ARGV[2]) / 1000
local requested = tonumber(ARGV[3])
local state = redis.call('HMGET', KEYS[1], 'tokens', 'last_ms')
local tokens = tonumber(state[1])
local last_ms = tonumber(state[2])
if tokens == nil then tokens = capacity end
if last_ms == nil then last_ms = now_ms end
if now_ms > last_ms then
  tokens = math.min(capacity, tokens + (now_ms - last_ms) * refill_per_ms)
end
local allowed = 0
local retry_ms = 0
if tokens >= requested then
  tokens = tokens - requested
  allowed = 1
elseif refill_per_ms > 0 then
  retry_ms = math.ceil((requested - tokens) / refill_per_ms)
else
  retry_ms = 60000
end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'last_ms', now_ms)
local ttl_ms = math.max(1000, math.ceil((capacity / math.max(refill_per_ms, 0.000001)) * 2))
redis.call('PEXPIRE', KEYS[1], ttl_ms)
return {allowed, math.floor(tokens), retry_ms}
`

type RateDecision struct {
	Allowed    bool
	Remaining  int64
	RetryAfter time.Duration
}

type RedisControls struct {
	client *redis.Client
	secret []byte
	script *redis.Script
}

func NewRedisControls(address, cacheSecret string) *RedisControls {
	return &RedisControls{
		client: redis.NewClient(&redis.Options{Addr: address, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second}),
		secret: []byte(cacheSecret),
		script: redis.NewScript(tokenBucketLua),
	}
}

func (r *RedisControls) Close() error { return r.client.Close() }

func (r *RedisControls) Ping(ctx context.Context) error {
	if err := r.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis unavailable: %w", err)
	}
	return nil
}

func (r *RedisControls) Allow(ctx context.Context, userID string, capacity int, refillPerSecond float64) (RateDecision, error) {
	if capacity <= 0 || refillPerSecond <= 0 {
		return RateDecision{}, fmt.Errorf("invalid rate policy")
	}
	result, err := r.script.Run(ctx, r.client, []string{"mg:v1:rl:" + userID}, capacity, strconv.FormatFloat(refillPerSecond, 'f', 6, 64), 1).Slice()
	if err != nil {
		return RateDecision{}, fmt.Errorf("rate limiter failed closed: %w", err)
	}
	if len(result) != 3 {
		return RateDecision{}, fmt.Errorf("rate limiter returned %d values", len(result))
	}
	allowed, err := redisInt(result[0])
	if err != nil {
		return RateDecision{}, err
	}
	remaining, err := redisInt(result[1])
	if err != nil {
		return RateDecision{}, err
	}
	retryMilliseconds, err := redisInt(result[2])
	if err != nil {
		return RateDecision{}, err
	}
	return RateDecision{Allowed: allowed == 1, Remaining: remaining, RetryAfter: time.Duration(retryMilliseconds) * time.Millisecond}, nil
}

func (r *RedisControls) CacheGet(ctx context.Context, userID, strategy, text string) ([]byte, bool, error) {
	value, err := r.client.Get(ctx, r.cacheKey(userID, strategy, text)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cache get: %w", err)
	}
	return value, true, nil
}

func (r *RedisControls) CacheSet(ctx context.Context, userID, strategy, text string, value []byte, ttl time.Duration) error {
	if ttl <= 0 || ttl > 24*time.Hour {
		return fmt.Errorf("cache TTL must be between zero and 24 hours")
	}
	if err := r.client.Set(ctx, r.cacheKey(userID, strategy, text), value, ttl).Err(); err != nil {
		return fmt.Errorf("cache set: %w", err)
	}
	return nil
}

func (r *RedisControls) cacheKey(userID, strategy, text string) string {
	mac := hmac.New(sha256.New, r.secret)
	_, _ = mac.Write([]byte(userID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(strategy))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(text))
	return "mg:v1:cache:" + hex.EncodeToString(mac.Sum(nil))
}

func (r *RedisControls) FlushForTest(ctx context.Context) error {
	return r.client.FlushDB(ctx).Err()
}

func redisInt(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	default:
		return 0, fmt.Errorf("unexpected redis integer type %T", value)
	}
}
