-- name: BumpRateLimit :one
INSERT INTO rate_limits (rl_key, window_start, count) VALUES ($1, $2, 1)
ON CONFLICT (rl_key, window_start) DO UPDATE SET count = rate_limits.count + 1
RETURNING count;

-- name: ExpireRateLimits :execrows
DELETE FROM rate_limits WHERE window_start < $1;
