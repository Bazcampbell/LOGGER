// logger.go
//
// One logging package for the whole suite. An application initialises its
// Postgres pool and reads its Telegram settings, hands both to Init, and from
// then on logs typed values from anywhere in the tree:
//
//	logger.Init(logger.Config{
//	    Application:   "pegasus",
//	    DefaultUserID: adminUserID,
//	    DB:            db.Pool(),
//	    Telegram:      &logger.TelegramSetup{...},
//	})
//	defer logger.Stop()
//
//	logger.Info(logger.InfoLog{Message: "server started"})
//	logger.Error(logger.ErrorLog{Message: "bet failed", Error: err})
//
// Level policy:
//
//	Debug — lifecycle, token refresh success, pool ops, every race message
//	Info  — first-time race, triple-s connect/subscribe, server start
//	Bet   — bet successfully placed
//	Warn  — token refresh failed, connection lost, reconnect failed
//	Error — decode failure, strategy error, http 5xx, startup failures
//
// Output:
//   - stderr (slog.TextHandler) — always, from process start.
//   - Postgres logs table — when Config.DB is set. Entries fan out to a
//     buffered channel drained by a background goroutine that bulk-inserts.
//   - Telegram — when Config.Telegram is set, routed per level to a channel,
//     rate limited and deduped.
//
// Both async sinks drop rather than block when their queue is full: logging
// must never stall the application.
//
// The level is read from the environment — <APPLICATION>_LOG_LEVEL, else
// LOG_LEVEL, else info — so it can be changed with an .env edit and a restart.

package logger

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bazcampbell/bazbet_logger/telegram"
)

// Logger owns the stderr handler and its sinks. Most applications never touch
// this type directly — Init installs one as the package default and the
// package-level functions delegate to it.
type Logger struct {
	slog *slog.Logger

	application   string
	defaultUserID string

	db sink
	tg sink

	stopOnce sync.Once
}

// sink is the narrow contract emit needs. Both real sinks are asynchronous:
// enqueue must never block, dropping instead when the queue is full.
type sink interface {
	enqueue(Entry)
	stop()
}

var std atomic.Pointer[Logger]

func init() {
	// Anything logged before Init — config parsing, pool setup, and any
	// failure therein — still reaches stderr rather than vanishing.
	std.Store(bootstrap())
}

// bootstrap builds a stderr-only logger with no sinks.
func bootstrap() *Logger {
	return &Logger{slog: slog.New(newTextHandler(resolveLevel("")))}
}

// resolveLevel reads the level from the environment. It is deployment config,
// not application wiring, so it deliberately isn't part of Config.
func resolveLevel(application string) slog.Level {
	v := ""
	if application != "" {
		v = os.Getenv(strings.ToUpper(application) + "_LOG_LEVEL")
	}
	if v == "" {
		v = os.Getenv("LOG_LEVEL")
	}

	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "bet":
		return LevelBet
	default:
		return slog.LevelInfo
	}
}

func newTextHandler(level slog.Level) slog.Handler {
	return slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				if lvl, ok := a.Value.Any().(slog.Level); ok && lvl == LevelBet {
					a.Value = slog.StringValue("BET")
				}
			}
			return a
		},
	})
}

// New builds a Logger and starts its sinks. Prefer Init unless you genuinely
// need more than one Logger in a process.
//
// A Telegram client that fails to construct is not fatal: the Logger is
// returned with the remaining sinks live, alongside the error, so a bad bot
// token degrades logging instead of failing startup.
func New(cfg Config) (*Logger, error) {
	setup := cfg.Setup.withDefaults()

	l := &Logger{
		slog:          slog.New(newTextHandler(resolveLevel(cfg.Application))),
		application:   cfg.Application,
		defaultUserID: cfg.DefaultUserID,
	}

	if cfg.DB != nil {
		l.db = newDBSink(cfg.DB, setup)
	}

	var tgErr error
	if cfg.Telegram.enabled() {
		client, err := telegram.NewTelegramClient(cfg.Telegram.BotToken)
		if err != nil {
			tgErr = err
		} else {
			l.tg = newTGSink(client, cfg.Telegram, setup)
		}
	}

	return l, tgErr
}

// Init builds a Logger from cfg and installs it as the package default, so
// logger.Info and friends work from anywhere.
//
// Any previously installed default is stopped first, so calling Init twice
// (a settings reload, say) doesn't leak sink goroutines.
//
// A non-nil error means Telegram specifically failed to initialise; the logger
// is installed and working regardless. Callers usually want to warn and
// continue rather than abort startup.
func Init(cfg Config) error {
	l, err := New(cfg)

	if old := std.Swap(l); old != nil {
		old.Stop()
	}
	// Third-party code logging through plain slog still reaches stderr. It
	// does not reach the DB or Telegram sinks — those are fed by the typed
	// API, which a generic slog.Record can't satisfy.
	slog.SetDefault(l.slog)

	return err
}

// Default returns the installed package logger.
func Default() *Logger { return std.Load() }

// SetDefault installs l as the package logger. The previous default is NOT
// stopped — use Init if you want that handled.
func SetDefault(l *Logger) {
	if l == nil {
		return
	}
	std.Store(l)
	slog.SetDefault(l.slog)
}

// Stop drains and closes the default logger's sinks, then leaves a
// stderr-only logger installed so late logs during shutdown still print.
// Call it before closing the DB pool — the final batch needs a live pool.
func Stop() {
	if old := std.Swap(bootstrap()); old != nil {
		old.Stop()
	}
}

// Stop flushes and closes this Logger's sinks. Safe to call more than once.
func (l *Logger) Stop() {
	l.stopOnce.Do(func() {
		if l.db != nil {
			l.db.stop()
		}
		if l.tg != nil {
			l.tg.stop()
		}
	})
}

// Slog exposes the underlying *slog.Logger, for handing to libraries that
// take one. It writes to stderr only.
func (l *Logger) Slog() *slog.Logger { return l.slog }

// emit renders one payload to stderr and fans it out to the sinks.
//
// runtime.Callers(3, ...) captures the PC of whoever called the public
// wrapper: frame 1 is emit itself, frame 2 is the wrapper (either the
// package-level function or the method), frame 3 is the real call site. Both
// wrapper paths are exactly one frame deep for this reason — a package-level
// function must call emit directly and never the method, or every log line's
// source would point back into this file. TestCallerSourceIsCallSite guards
// this.
func (l *Logger) emit(level slog.Level, p payload) {
	ctx := context.Background()
	if !l.slog.Enabled(ctx, level) {
		return
	}

	var pcs [1]uintptr
	runtime.Callers(3, pcs[:])
	pc := pcs[0]

	now := time.Now()

	r := slog.NewRecord(now, level, p.message(), pc)
	r.AddAttrs(p.attrs()...)
	_ = l.slog.Handler().Handle(ctx, r)

	if l.db == nil && l.tg == nil {
		return
	}

	e := newEntry(p, level, now, pc, l.application, l.defaultUserID)

	if l.db != nil {
		l.db.enqueue(e)
	}
	if l.tg != nil {
		l.tg.enqueue(e)
	}
}

func (l *Logger) Debug(v InfoLog)  { l.emit(slog.LevelDebug, v) }
func (l *Logger) Info(v InfoLog)   { l.emit(slog.LevelInfo, v) }
func (l *Logger) Warn(v ErrorLog)  { l.emit(slog.LevelWarn, v) }
func (l *Logger) Error(v ErrorLog) { l.emit(slog.LevelError, v) }
func (l *Logger) Bet(v BetLog)     { l.emit(LevelBet, v) }

func Debug(v InfoLog)  { std.Load().emit(slog.LevelDebug, v) }
func Info(v InfoLog)   { std.Load().emit(slog.LevelInfo, v) }
func Warn(v ErrorLog)  { std.Load().emit(slog.LevelWarn, v) }
func Error(v ErrorLog) { std.Load().emit(slog.LevelError, v) }
func Bet(v BetLog)     { std.Load().emit(LevelBet, v) }
