// sink_telegram.go
//
// Streams Info/Warn/Error/Bet logs to their configured channels. Delivery runs
// on a single background goroutine draining a bounded queue, so slow Telegram
// round-trips never block the app and sends stay naturally serialised.
//
// Two things keep an error storm from turning into a 429 storm:
//
//   - a token bucket per channel plus a global one (see ratelimit.go), since
//     Telegram's limits are per bot and the suite's processes can't see each
//     other's traffic;
//   - a dedupe window that collapses identical repeated messages into one
//     "+N more" summary rather than sending each.

package logger

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/bazcampbell/bazbet_logger/telegram"
)

// Sender is the send-side contract the sink needs. Exported so callers can
// substitute a fake in tests.
type Sender interface {
	SendLog(text string, channelID int64) error
}

type tgSink struct {
	queue  chan Entry
	client Sender
	setup  *TelegramSetup

	limiter *rateLimiter

	dedupeWindow time.Duration
	suppressed   map[string]*suppression

	wg       sync.WaitGroup
	stopOnce sync.Once
}

// suppression tracks one deduped message: when it was first sent and how many
// identical repeats have been swallowed since.
type suppression struct {
	firstSent time.Time
	count     int
	channelID int64
	sample    Entry
}

func newTGSink(client Sender, setup *TelegramSetup, s LoggerSetup) *tgSink {
	t := &tgSink{
		queue:        make(chan Entry, s.TelegramQueueSize),
		client:       client,
		setup:        setup,
		limiter:      newRateLimiter(),
		dedupeWindow: s.DedupeWindow,
		suppressed:   make(map[string]*suppression),
	}
	t.wg.Add(1)
	go t.run()
	return t
}

func (t *tgSink) enqueue(e Entry) {
	if t.setup.channelFor(e.LogType) == nil {
		return
	}
	select {
	case t.queue <- e:
	default:
		// Same policy as the DB sink: never block the app on Telegram.
	}
}

func (t *tgSink) stop() {
	t.stopOnce.Do(func() {
		close(t.queue)
		t.wg.Wait()
	})
}

func (t *tgSink) run() {
	defer t.wg.Done()

	// The sweep interval only bounds how promptly a "+N more" summary lands;
	// a quarter of the window is responsive enough without busy-waking.
	interval := t.dedupeWindow / 4
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case e, ok := <-t.queue:
			if !ok {
				t.sweep(true)
				return
			}
			t.dispatch(e)
		case <-ticker.C:
			t.sweep(false)
		}
	}
}

// dispatch sends one entry, unless an identical one is already inside its
// dedupe window — in which case it's counted and swallowed.
func (t *tgSink) dispatch(e Entry) {
	ch := t.setup.channelFor(e.LogType)
	if ch == nil {
		return
	}

	if t.dedupeWindow > 0 {
		key := dedupeKey(e)
		now := time.Now()

		if s, ok := t.suppressed[key]; ok && now.Sub(s.firstSent) < t.dedupeWindow {
			s.count++
			return
		}

		t.suppressed[key] = &suppression{
			firstSent: now,
			channelID: *ch,
			sample:    e,
		}
	}

	t.send(*ch, formatTelegram(e))
}

// sweep expires dedupe windows, emitting a summary for any that swallowed
// repeats. force drains everything regardless of age, for shutdown.
func (t *tgSink) sweep(force bool) {
	if t.dedupeWindow <= 0 {
		return
	}
	now := time.Now()

	for key, s := range t.suppressed {
		if !force && now.Sub(s.firstSent) < t.dedupeWindow {
			continue
		}
		if s.count > 0 {
			t.send(s.channelID, formatSuppressed(s.sample, s.count))
		}
		delete(t.suppressed, key)
	}
}

func (t *tgSink) send(channelID int64, text string) {
	t.limiter.wait(channelID)

	err := t.client.SendLog(text, channelID)
	if err == nil {
		return
	}

	// Telegram told us exactly how long to back off — honour it so the next
	// send from this process doesn't compound the problem.
	if d, ok := telegram.RetryAfter(err); ok {
		t.limiter.penalise(channelID, d)
	}

	// Deliberately raw stderr, NOT this package's logger: an Error here would
	// be re-queued into this same sink, fail again, and loop forever.
	fmt.Fprintf(os.Stderr, "logger: telegram send failed (channel %d): %v\n", channelID, err)
}

// dedupeKey identifies "the same message again". The trace is deliberately
// excluded — the same error from the same call site is what we want collapsed.
func dedupeKey(e Entry) string {
	return e.LogType + "\x00" + e.Message + "\x00" + e.Error + "\x00" + e.ProcessID
}
