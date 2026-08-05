// config.go

package logger

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Application   string
	DefaultUserID string // sent when no user ID is provided

	DB       *pgxpool.Pool  // optional
	Telegram *TelegramSetup // optional

	Setup LoggerSetup
}

type TelegramSetup struct {
	BotToken string

	InfoLogChannelID  *int64
	WarnLogChannelID  *int64
	ErrorLogChannelID *int64
	BetLogChannelID   *int64

	DefaultChannelID *int64 // if above channels not set, default here
}

func (t *TelegramSetup) channelFor(logType string) *int64 {
	var id *int64
	switch logType {
	case "INFO":
		id = t.InfoLogChannelID
	case "WARN":
		id = t.WarnLogChannelID
	case "ERROR":
		id = t.ErrorLogChannelID
	case "BET":
		id = t.BetLogChannelID
	default:
		return nil
	}
	if id == nil {
		id = t.DefaultChannelID
	}
	return id
}

func (t *TelegramSetup) enabled() bool {
	return t != nil && t.BotToken != "" &&
		(t.InfoLogChannelID != nil || t.WarnLogChannelID != nil ||
			t.ErrorLogChannelID != nil || t.BetLogChannelID != nil ||
			t.DefaultChannelID != nil)
}

type LoggerSetup struct {
	// QueueSize bounds the DB sink's channel
	// Full queue drops the record to not block application
	QueueSize int

	// force flush once this many entries are buffered.
	BatchSize int

	// force flush this often regardless of batch size.
	FlushInterval time.Duration

	// bounds single batch INSERT.
	InsertTimeout time.Duration

	// TelegramQueueSize bounds the Telegram sink's channel.
	TelegramQueueSize int
	// DedupeWindow collapses identical repeated messages sent to Telegram
	// within this window into a single "+N more" summary. Zero disables.
	DedupeWindow time.Duration
}

const (
	defaultQueueSize     = 1000
	defaultBatchSize     = 100
	defaultFlushInterval = 5 * time.Second
	defaultInsertTimeout = 5 * time.Second
	defaultDedupeWindow  = 60 * time.Second
)

func (s LoggerSetup) withDefaults() LoggerSetup {
	if s.QueueSize <= 0 {
		s.QueueSize = envInt("LOG_QUEUE_SIZE", defaultQueueSize)
	}
	if s.BatchSize <= 0 {
		s.BatchSize = envInt("LOG_BATCH_SIZE", defaultBatchSize)
	}
	if s.FlushInterval <= 0 {
		ms := envInt("LOG_FLUSH_INTERVAL_MS", int(defaultFlushInterval/time.Millisecond))
		s.FlushInterval = time.Duration(ms) * time.Millisecond
	}
	if s.InsertTimeout <= 0 {
		s.InsertTimeout = defaultInsertTimeout
	}
	if s.TelegramQueueSize <= 0 {
		s.TelegramQueueSize = envInt("LOG_TG_QUEUE_SIZE", defaultQueueSize)
	}
	if s.DedupeWindow == 0 {
		ms := envInt("LOG_TG_DEDUPE_MS", int(defaultDedupeWindow/time.Millisecond))
		s.DedupeWindow = time.Duration(ms) * time.Millisecond
	}
	return s
}
