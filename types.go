// types.go

package logger

import (
	"log/slog"
	"time"
)

const LevelBet slog.Level = 2

type RaceDetails struct {
	Venue        string
	RaceNumber   int
	RunnerNumber int
	RunnerName   string
}

type InfoLog struct {
	UserID    string
	Username  string
	ProcessID string

	Message string

	RaceDetails *RaceDetails
}

type ErrorLog struct {
	UserID    string
	Username  string
	ProcessID string

	Message  string
	Error    error
	Request  any
	Response any
	Trace    *string

	RaceDetails *RaceDetails
}

type BetLog struct {
	UserID    string
	Username  string
	ProcessID string

	RaceDetails *RaceDetails

	Endpoint string // betfair, betmatic, tote
	Market   string

	Stake     float64
	Odds      float64
	TargetLia float64
	Result    string // WIN, LOSE, VOID, DH

	RaceState string

	Message  string
	Request  any
	Response any
}

// handed to each sink
type Entry struct {
	Time        time.Time
	LogType     string
	Application string
	Message     string
	UserID      string
	Username    string
	ProcessID   string
	RaceDetails *RaceDetails
	Trace       *string // JSONB
	Bet         *string // JSONB, populated only for BET-level logs
	Request     *string // JSONB
	Response    *string // JSONB

	// Error has no column of its own in the logs table, so it rides in the
	// trace JSONB under an "error" key. Kept here as a plain string because
	// the Telegram card renders it as its own block.
	Error string

	// bet is the original payload, kept so the Telegram formatter doesn't have
	// to unmarshal the JSONB it was just handed. Never persisted directly.
	bet *BetLog
}

// payload is what the five logging functions accept. Implemented by the three
// consumer types and nothing else — the interface is unexported so the set
// stays closed.
type payload interface {
	message() string
	attrs() []slog.Attr
	fill(*Entry)
}

// --- InfoLog ---

func (l InfoLog) message() string { return l.Message }

func (l InfoLog) attrs() []slog.Attr {
	a := make([]slog.Attr, 0, 7)
	a = appendStr(a, "user_id", l.UserID)
	a = appendStr(a, "username", l.Username)
	a = appendStr(a, "process_id", l.ProcessID)
	return appendRaceAttrs(a, l.RaceDetails)
}

func (l InfoLog) fill(e *Entry) {
	e.UserID = l.UserID
	e.Username = l.Username
	e.ProcessID = l.ProcessID
	e.RaceDetails = l.RaceDetails
}

// --- ErrorLog ---

func (l ErrorLog) message() string {
	if l.Message != "" {
		return l.Message
	}
	if l.Error != nil {
		return l.Error.Error()
	}
	return ""
}

func (l ErrorLog) attrs() []slog.Attr {
	a := make([]slog.Attr, 0, 8)
	a = appendStr(a, "user_id", l.UserID)
	a = appendStr(a, "username", l.Username)
	a = appendStr(a, "process_id", l.ProcessID)
	a = appendRaceAttrs(a, l.RaceDetails)
	if l.Error != nil {
		a = append(a, slog.String("error", l.Error.Error()))
	}
	return a
}

func (l ErrorLog) fill(e *Entry) {
	e.UserID = l.UserID
	e.Username = l.Username
	e.ProcessID = l.ProcessID
	e.RaceDetails = l.RaceDetails

	if l.Error != nil {
		e.Error = l.Error.Error()
	}
	e.Request = toJSONBString(l.Request)
	e.Response = toJSONBString(l.Response)
}

// --- BetLog ---

func (l BetLog) message() string { return l.Message }

func (l BetLog) attrs() []slog.Attr {
	a := make([]slog.Attr, 0, 12)
	a = appendStr(a, "user_id", l.UserID)
	a = appendStr(a, "username", l.Username)
	a = appendStr(a, "process_id", l.ProcessID)
	a = appendRaceAttrs(a, l.RaceDetails)
	a = appendStr(a, "market", l.Market)
	a = appendStr(a, "endpoint", l.Endpoint)
	a = appendFloat(a, "stake", l.Stake)
	a = appendFloat(a, "odds", l.Odds)
	a = appendStr(a, "result", l.Result)
	return a
}

func (l BetLog) fill(e *Entry) {
	e.UserID = l.UserID
	e.Username = l.Username
	e.ProcessID = l.ProcessID
	e.RaceDetails = l.RaceDetails

	bet := l
	e.bet = &bet
	e.Bet = betJSONB(l, e.Time)

	e.Request = toJSONBString(l.Request)
	e.Response = toJSONBString(l.Response)
}

// --- attr helpers ---

func appendRaceAttrs(a []slog.Attr, r *RaceDetails) []slog.Attr {
	if r == nil {
		return a
	}
	a = appendStr(a, "venue", r.Venue)
	a = appendInt(a, "race", r.RaceNumber)
	a = appendInt(a, "runner", r.RunnerNumber)
	a = appendStr(a, "runner_name", r.RunnerName)
	return a
}

func appendStr(a []slog.Attr, k, v string) []slog.Attr {
	if v == "" {
		return a
	}
	return append(a, slog.String(k, v))
}

func appendInt(a []slog.Attr, k string, v int) []slog.Attr {
	if v == 0 {
		return a
	}
	return append(a, slog.Int(k, v))
}

func appendFloat(a []slog.Attr, k string, v float64) []slog.Attr {
	if v == 0 {
		return a
	}
	return append(a, slog.Float64(k, v))
}
