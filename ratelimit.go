// ratelimit.go
//
// Telegram enforces rate limits per *bot*, not per client connection. Every
// application in the suite sends through the same bot token from its own
// process, and those processes don't coordinate — so each one has to be
// conservative on its own. The published ceilings are roughly 30 messages/sec
// globally and 20 messages/min into any one group or channel; we sit under
// both.

package logger

import (
	"sync"
	"time"
)

const (
	globalRate  = 25.0 / 1.0 // messages per second across all channels
	globalBurst = 25.0
	chatRate    = 18.0 / 60.0 // messages per second into one channel
	chatBurst   = 18.0
)

// bucket is a standard token bucket. Tokens refill continuously at rate per
// second and cap at burst.
type bucket struct {
	tokens float64
	burst  float64
	rate   float64
	last   time.Time
}

func newBucket(rate, burst float64, now time.Time) *bucket {
	return &bucket{tokens: burst, burst: burst, rate: rate, last: now}
}

func (b *bucket) refill(now time.Time) {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
}

// delay reports how long until a token is available, without consuming one.
func (b *bucket) delay(now time.Time) time.Duration {
	b.refill(now)
	if b.tokens >= 1 {
		return 0
	}
	return time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}

func (b *bucket) consume() { b.tokens-- }

// rateLimiter paces sends across one global bucket and a per-channel bucket.
type rateLimiter struct {
	mu      sync.Mutex
	global  *bucket
	perChat map[int64]*bucket

	// sleep is swappable so tests don't have to wait in real time.
	sleep func(time.Duration)
	now   func() time.Time
}

func newRateLimiter() *rateLimiter {
	now := time.Now()
	return &rateLimiter{
		global:  newBucket(globalRate, globalBurst, now),
		perChat: make(map[int64]*bucket),
		sleep:   time.Sleep,
		now:     time.Now,
	}
}

// wait blocks until a message may be sent to chatID, then consumes a token
// from both buckets. Called only from the Telegram sink's single drain
// goroutine, so blocking here never touches the application's own goroutines —
// backpressure shows up as a full queue, and a full queue drops.
func (r *rateLimiter) wait(chatID int64) {
	for {
		r.mu.Lock()
		now := r.now()

		chat, ok := r.perChat[chatID]
		if !ok {
			chat = newBucket(chatRate, chatBurst, now)
			r.perChat[chatID] = chat
		}

		d := r.global.delay(now)
		if cd := chat.delay(now); cd > d {
			d = cd
		}

		if d <= 0 {
			r.global.consume()
			chat.consume()
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()

		r.sleep(d)
	}
}

// penalise burns the given duration against a channel after Telegram replies
// 429 with a retry_after, so we stop hammering a channel we've already been
// told to back off from.
func (r *rateLimiter) penalise(chatID int64, d time.Duration) {
	if d <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	chat, ok := r.perChat[chatID]
	if !ok {
		chat = newBucket(chatRate, chatBurst, now)
		r.perChat[chatID] = chat
	}
	// Empty the bucket and push its refill clock forward by the penalty, so
	// delay() returns at least d for the next send.
	chat.refill(now)
	chat.tokens = 0
	chat.last = now.Add(d)
}
