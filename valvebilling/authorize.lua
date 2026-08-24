
local cost    = tonumber(ARGV[1])
local cuCost  = tonumber(ARGV[2])
local dayLim  = tonumber(ARGV[3])
local cuSLim  = tonumber(ARGV[4])
local cuDLim  = tonumber(ARGV[5])
local thresh  = tonumber(ARGV[6])
local fullCps = tonumber(ARGV[7])
local slowCps = tonumber(ARGV[8])
local fullRps = tonumber(ARGV[9])
local slowRps = tonumber(ARGV[10])
local keyRps  = tonumber(ARGV[11])
local nMeth   = tonumber(ARGV[12])

-- Mode-1 per-request lock. Runs first so a held lock cannot burn the
-- account's per-second or per-day budget.
if redis.call('EXISTS', KEYS[1]) == 1 then
  return { 'per_request_lock', 'NONE' }
end

local dayCount = tonumber(redis.call('GET', KEYS[2])) or 0
if dayLim > 0 and dayCount + 1 > dayLim then return { 'rate_day', 'NONE' } end

local cuSec = tonumber(redis.call('GET', KEYS[3])) or 0
if cuSLim > 0 and cuSec + cuCost > cuSLim then return { 'cu_rate_second', 'NONE' } end

local cuDay = tonumber(redis.call('GET', KEYS[4])) or 0
if cuDLim > 0 and cuDay + cuCost > cuDLim then return { 'cu_rate_day', 'NONE' } end

if redis.call('GET', KEYS[8]) == '1' then return { 'closing', 'NONE' } end

local ceiling = tonumber(redis.call('GET', KEYS[5])) or 0
local pending = tonumber(redis.call('GET', KEYS[6])) or 0
local spend   = tonumber(redis.call('GET', KEYS[7])) or 0
local effective = ceiling + pending - spend
if effective - cost < 0 then return { 'no_credits', 'NONE' } end

local tier     = 'FULL'
local cpsLimit = fullCps
local tierRps  = fullRps
if effective < thresh then
  tier     = 'SLOW'
  cpsLimit = slowCps
  tierRps  = slowRps
end

-- cost > 0 guard: the cps bucket is per ACCOUNT, shared by every key on
-- it. A zero-cost call moves the bucket by nothing, so without this guard
-- it would still trip on OTHER keys' spend. Mirrors SPEND_LUA.
local chargeCps = cpsLimit > 0 and cost > 0
if chargeCps then
  local cpsCount = tonumber(redis.call('GET', KEYS[9])) or 0
  if cpsCount + cost > cpsLimit then
    -- Self-heal on the REJECT path too, not just the commit path. This
    -- bucket's key is fixed (no time component), so unlike every other
    -- counter here a lost TTL never ages out: the bucket stays at or over
    -- the limit and rejects the account forever. That is the 2026-08-07
    -- wedge, which took the x402 heartbeat down for 44 hours. An
    -- over-limit request is exactly the case that must still repair it.
    redis.call('EXPIRE', KEYS[9], 2, 'NX')
    return { 'cps_throttle', tier }
  end
end

-- Per-(key, method) per-second, ip-mode public tier only. Checked before
-- the aggregate gate so a per-method trip does not consume an aggregate
-- slot.
for i = 1, nMeth do
  local base   = 12 + (i - 1) * 3
  local mNow   = tonumber(redis.call('GET', ARGV[base + 1])) or 0
  local mBy    = tonumber(ARGV[base + 2])
  local mLimit = tonumber(ARGV[base + 3])
  if mLimit > 0 and mNow + mBy > mLimit then
    return { 'rate_second_method', tier }
  end
end

-- Aggregate per-second, intersected with the tier cap. A zero per-key
-- limit means "no per-key cap" and defers entirely to the tier.
local effRps = tierRps
if keyRps > 0 and keyRps < tierRps then effRps = keyRps end
if effRps > 0 then
  local secCount = tonumber(redis.call('GET', KEYS[10])) or 0
  if secCount + 1 > effRps then return { 'rate_second', tier } end
end

-- Every gate passed. Commit.
if dayLim > 0 then
  redis.call('INCR', KEYS[2])
  redis.call('EXPIRE', KEYS[2], 86400, 'NX')
end
if cuSLim > 0 then
  redis.call('INCRBYFLOAT', KEYS[3], cuCost)
  redis.call('EXPIRE', KEYS[3], 2, 'NX')
end
if cuDLim > 0 then
  redis.call('INCRBYFLOAT', KEYS[4], cuCost)
  redis.call('EXPIRE', KEYS[4], 86400, 'NX')
end
if chargeCps then
  redis.call('INCRBY', KEYS[9], cost)
  redis.call('EXPIRE', KEYS[9], 2, 'NX')
end
for i = 1, nMeth do
  local base = 12 + (i - 1) * 3
  redis.call('INCRBY', ARGV[base + 1], tonumber(ARGV[base + 2]))
  redis.call('EXPIRE', ARGV[base + 1], 2, 'NX')
end
if effRps > 0 then
  redis.call('INCR', KEYS[10])
  redis.call('EXPIRE', KEYS[10], 2, 'NX')
end

return { 'ok', tier }
