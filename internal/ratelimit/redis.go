package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// reserveScript performs the whole reserve decision atomically: refill the
// per-minute token bucket, check the monthly budget, and take the reservation.
//
// It is one script rather than several round trips because a limiter that is
// not atomic is not a limiter — two replicas interleaving read-modify-write
// will both admit a request that only one of them should have.
//
//	KEYS[1] per-minute token bucket hash
//	KEYS[2] monthly consumption counter
//	ARGV[1] capacity            ARGV[2] refill rate per second
//	ARGV[3] now (ms)            ARGV[4] requested tokens
//	ARGV[5] monthly budget      ARGV[6] bucket ttl (s)
//	ARGV[7] month ttl (s)
//
// Returns {allowed, remaining, retry_after_ms, reason}
//
//	reason: 0 allowed, 1 per-minute quota, 2 monthly budget
var reserveScript = redis.NewScript(`
local capacity   = tonumber(ARGV[1])
local rate       = tonumber(ARGV[2])
local now        = tonumber(ARGV[3])
local requested  = tonumber(ARGV[4])
local budget     = tonumber(ARGV[5])
local bucket_ttl = tonumber(ARGV[6])
local month_ttl  = tonumber(ARGV[7])

if budget > 0 then
  local consumed = tonumber(redis.call('GET', KEYS[2]) or '0')
  if consumed + requested > budget then
    return {0, math.max(budget - consumed, 0), 3600000, 2}
  end
end

if capacity <= 0 then
  return {1, -1, 0, 0}
end

local state = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts = tonumber(state[2])
if tokens == nil then
  tokens = capacity
  ts = now
end
local elapsed = math.max(now - ts, 0) / 1000.0
tokens = math.min(capacity, tokens + elapsed * rate)

if tokens < requested then
  local deficit = requested - tokens
  local retry = 60000
  if rate > 0 then retry = math.ceil(deficit / rate * 1000) end
  redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
  redis.call('EXPIRE', KEYS[1], bucket_ttl)
  return {0, math.floor(tokens), retry, 1}
end

tokens = tokens - requested
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now, 'cap', capacity, 'rate', rate)
redis.call('EXPIRE', KEYS[1], bucket_ttl)
if month_ttl > 0 then redis.call('EXPIRE', KEYS[2], month_ttl) end
return {1, math.floor(tokens), 0, 0}
`)

// settleScript returns unused tokens to the bucket and records the actual
// consumption against the monthly counter.
//
//	KEYS[1] bucket hash   KEYS[2] monthly counter
//	ARGV[1] reserved      ARGV[2] actual
//	ARGV[3] now (ms)      ARGV[4] month ttl (s)   ARGV[5] bucket ttl (s)
var settleScript = redis.NewScript(`
local reserved  = tonumber(ARGV[1])
local actual    = tonumber(ARGV[2])
local now       = tonumber(ARGV[3])
local month_ttl = tonumber(ARGV[4])
local bucket_ttl= tonumber(ARGV[5])

do
  -- The bucket carries its own capacity and refill rate, written at reserve
  -- time, so settle does not need the agent's limits re-fetched on this path.
  local state = redis.call('HMGET', KEYS[1], 'tokens', 'ts', 'cap', 'rate')
  local tokens = tonumber(state[1])
  local ts = tonumber(state[2])
  local capacity = tonumber(state[3]) or 0
  local rate = tonumber(state[4]) or 0
  if tokens ~= nil and capacity > 0 then
    -- Apply the refill accrued while the request was in flight BEFORE
    -- returning the unused reservation, then move ts to now. Skipping this
    -- step silently destroys (now - ts) * rate tokens on every request, which
    -- for a ten-minute stream is ten minutes of an agent's entire allowance.
    if ts ~= nil and rate > 0 then
      tokens = math.min(capacity, tokens + math.max(now - ts, 0) / 1000.0 * rate)
    end
    -- The overshoot is charged unconditionally and the bucket may go negative,
    -- matching the in-memory limiter: an agent that under-declares carries the
    -- debt into the next window instead of escaping it.
    tokens = math.min(capacity, tokens + (reserved - actual))
    redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
    redis.call('EXPIRE', KEYS[1], bucket_ttl)
  end
end
local total = redis.call('INCRBY', KEYS[2], actual)
if month_ttl > 0 then redis.call('EXPIRE', KEYS[2], month_ttl) end
return total
`)

// requestScript is a plain fixed-capacity token bucket for requests per
// minute.
var requestScript = redis.NewScript(`
local capacity = tonumber(ARGV[1])
local rate     = tonumber(ARGV[2])
local now      = tonumber(ARGV[3])
local ttl      = tonumber(ARGV[4])
if capacity <= 0 then return {1, -1, 0} end
local state = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts = tonumber(state[2])
if tokens == nil then tokens = capacity; ts = now end
local elapsed = math.max(now - ts, 0) / 1000.0
tokens = math.min(capacity, tokens + elapsed * rate)
if tokens < 1 then
  local retry = 1000
  if rate > 0 then retry = math.ceil((1 - tokens) / rate * 1000) end
  redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
  redis.call('EXPIRE', KEYS[1], ttl)
  return {0, 0, retry}
end
tokens = tokens - 1
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', KEYS[1], ttl)
return {1, math.floor(tokens), 0}
`)

// Redis is the distributed Limiter used in production.
type Redis struct {
	client    redis.UniversalClient
	prefix    string
	now       func() time.Time
	monthTTL  time.Duration
	bucketTTL time.Duration
}

// RedisOptions configures the distributed limiter.
type RedisOptions struct {
	Client    redis.UniversalClient
	KeyPrefix string
	// MonthTTL bounds how long monthly counters persist after the month ends.
	MonthTTL time.Duration
	// BucketTTL expires idle buckets so an agent that stops calling does not
	// occupy memory forever.
	BucketTTL time.Duration
}

// NewRedis builds a distributed limiter.
func NewRedis(o RedisOptions) *Redis {
	if o.KeyPrefix == "" {
		o.KeyPrefix = "agentgate:rl"
	}
	if o.MonthTTL <= 0 {
		o.MonthTTL = 40 * 24 * time.Hour
	}
	if o.BucketTTL <= 0 {
		o.BucketTTL = 10 * time.Minute
	}
	return &Redis{client: o.Client, prefix: o.KeyPrefix, now: time.Now, monthTTL: o.MonthTTL, bucketTTL: o.BucketTTL}
}

func (r *Redis) reqKey(key string) string { return fmt.Sprintf("%s:req:{%s}", r.prefix, key) }
func (r *Redis) tokKey(key string) string { return fmt.Sprintf("%s:tok:{%s}", r.prefix, key) }
func (r *Redis) monthKeyS(key string) string {
	return fmt.Sprintf("%s:mon:{%s}:%s", r.prefix, key, monthKey(r.now()))
}

// AllowRequest consumes one request from the distributed request bucket.
func (r *Redis) AllowRequest(ctx context.Context, key string, limits Limits) (Result, error) {
	if limits.RequestsPerMinute <= 0 {
		return Result{Decision: DecisionAllow}, nil
	}
	now := r.now()
	out, err := requestScript.Run(ctx, r.client, []string{r.reqKey(key)},
		limits.RequestsPerMinute, float64(limits.RequestsPerMinute)/60,
		now.UnixMilli(), int(r.bucketTTL.Seconds()),
	).Int64Slice()
	if err != nil {
		return Result{}, err
	}
	res := Result{Limit: limits.RequestsPerMinute, Remaining: out[1], ResetAfter: resetAfter(now)}
	if out[0] == 1 {
		res.Decision = DecisionAllow
		return res, nil
	}
	res.Decision = DecisionLimit
	res.RetryAfter = time.Duration(out[2]) * time.Millisecond
	res.Reason = "requests per minute exceeded"
	return res, nil
}

// Reserve takes tokens from the distributed token bucket.
func (r *Redis) Reserve(ctx context.Context, key string, tokens int64, limits Limits) (Result, *Reservation, error) {
	now := r.now()
	out, err := reserveScript.Run(ctx, r.client,
		[]string{r.tokKey(key), r.monthKeyS(key)},
		limits.TokensPerMinute, float64(limits.TokensPerMinute)/60,
		now.UnixMilli(), tokens, limits.MonthlyTokenBudget,
		int(r.bucketTTL.Seconds()), int(r.monthTTL.Seconds()),
	).Int64Slice()
	if err != nil {
		return Result{}, nil, err
	}
	res := Result{Limit: limits.TokensPerMinute, Remaining: out[1], ResetAfter: resetAfter(now)}
	if out[0] == 1 {
		res.Decision = DecisionAllow
		return res, &Reservation{Key: key, Reserved: tokens, limiter: r}, nil
	}
	res.RetryAfter = time.Duration(out[2]) * time.Millisecond
	switch out[3] {
	case 2:
		res.Decision = DecisionBudget
		res.Limit = limits.MonthlyTokenBudget
		res.Reason = "monthly token budget exhausted"
	default:
		res.Decision = DecisionQuota
		res.Reason = "tokens per minute exceeded"
	}
	return res, nil, nil
}

// Settle reconciles a reservation against actual usage.
func (r *Redis) Settle(ctx context.Context, key string, reserved, actual int64) error {
	return settleScript.Run(ctx, r.client,
		[]string{r.tokKey(key), r.monthKeyS(key)},
		reserved, actual, r.now().UnixMilli(),
		int(r.monthTTL.Seconds()), int(r.bucketTTL.Seconds()),
	).Err()
}

// Usage reports monthly consumption.
func (r *Redis) Usage(ctx context.Context, key string) (int64, error) {
	v, err := r.client.Get(ctx, r.monthKeyS(key)).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

// Healthy pings the store.
func (r *Redis) Healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return r.client.Ping(ctx).Err() == nil
}

// Close releases the client.
func (r *Redis) Close() error { return r.client.Close() }
