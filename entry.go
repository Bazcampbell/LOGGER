// entry.go
//
// Building an Entry from a payload, plus the JSON encoding the JSONB columns
// need.
//
// JSON keys are written explicitly rather than via struct tags so the consumer
// types stay free of persistence concerns, and so the column shape can't drift
// when a field is renamed in Go.

package logger

import (
	"encoding/json"
	"log/slog"
	"runtime"
	"time"
)

// newEntry converts a payload into the row the sinks persist. Everything the
// consumer didn't set — application, timestamp, source location, the default
// user — is filled in here.
func newEntry(p payload, level slog.Level, at time.Time, pc uintptr, application, defaultUserID string) Entry {
	e := Entry{
		Time:        at,
		LogType:     levelString(level),
		Application: application,
		Message:     p.message(),
	}

	p.fill(&e)

	// The call site is always captured. An ErrorLog carrying its own Trace
	// adds to it rather than replacing it — losing the file and line to gain a
	// stack would be a bad trade.
	var extra *string
	if el, ok := p.(ErrorLog); ok {
		extra = el.Trace
	}
	e.Trace = traceJSON(pc, extra)

	if e.UserID == "" {
		e.UserID = defaultUserID
	}
	return e
}

// raceDetailsJSONB renders the race context. Zero-valued fields are omitted so
// the column doesn't fill with meaningless defaults; an entirely empty
// RaceDetails yields NULL rather than "{}".
func raceDetailsJSONB(r *RaceDetails) *string {
	if r == nil {
		return nil
	}

	m := make(map[string]any, 4)
	putStr(m, "venue", r.Venue)
	putInt(m, "race_number", r.RaceNumber)
	putInt(m, "runner_number", r.RunnerNumber)
	putStr(m, "runner_name", r.RunnerName)

	return marshalOrNil(m)
}

// betJSONB renders the bet-specific fields of a BetLog. placed_at is echoed in
// so the object is self-contained for analysis.
func betJSONB(b BetLog, at time.Time) *string {
	m := make(map[string]any, 8)
	putStr(m, "endpoint", b.Endpoint)
	putStr(m, "market", b.Market)
	putFloat(m, "stake", b.Stake)
	putFloat(m, "odds", b.Odds)
	putFloat(m, "target_lia", b.TargetLia)
	putStr(m, "result", b.Result)
	putStr(m, "race_state", b.RaceState)

	m["placed_at"] = at.UTC().Format(time.RFC3339Nano)

	return marshalOrNil(m)
}

func putStr(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func putInt(m map[string]any, k string, v int) {
	if v != 0 {
		m[k] = v
	}
}

func putFloat(m map[string]any, k string, v float64) {
	if v != 0 {
		m[k] = v
	}
}

func marshalOrNil(m map[string]any) *string {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

func levelString(l slog.Level) string {
	if l == LevelBet {
		return "BET"
	}
	return l.String()
}

// traceJSON renders the captured call site as {package, function, file, line},
// plus "trace" when the caller supplied one.
//
// Resolution goes through runtime.CallersFrames rather than runtime.FuncForPC:
// the latter doesn't expand inlined frames, so a call site inside an inlined
// function resolves to whatever it was inlined into. slog does the same.
func traceJSON(pc uintptr, extra *string) *string {
	m := make(map[string]any, 5)

	if pc != 0 {
		if frame, _ := runtime.CallersFrames([]uintptr{pc}).Next(); frame.Function != "" || frame.File != "" {
			pkg, fn := splitFuncName(frame.Function)
			m["package"] = pkg
			m["function"] = fn
			m["file"] = frame.File
			m["line"] = frame.Line
		}
	}

	if extra != nil && *extra != "" {
		m["trace"] = *extra
	}

	return marshalOrNil(m)
}

// dbMessage is what lands in the message column. There's no error column, so
// the error text is appended here — otherwise it would only survive when it
// happened to also be the message.
//
// Entry.Message already falls back to the error when no message was given, so
// the equality check stops that case rendering as "boom: boom".
func dbMessage(e Entry) string {
	switch {
	case e.Error == "" || e.Message == e.Error:
		return e.Message
	case e.Message == "":
		return e.Error
	default:
		return e.Message + ": " + e.Error
	}
}

// splitFuncName separates a fully qualified runtime function name into its
// package path and the function within it. The last "/" ends the path, and the
// first "." after that separates package from function — method names keep
// their receiver, e.g. "(*Engine).AddProcess".
//
//	"pegasus_betting/packages/engine.(*Engine).AddProcess"
//	"main.main"
func splitFuncName(name string) (pkg, fn string) {
	pkgPath := ""
	tail := name
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			pkgPath = name[:i+1]
			tail = name[i+1:]
			break
		}
	}
	for i := 0; i < len(tail); i++ {
		if tail[i] == '.' {
			return pkgPath + tail[:i], tail[i+1:]
		}
	}
	return pkgPath + tail, ""
}

// toJSONBString encodes an arbitrary value into a JSON string suitable for a
// JSONB column. Strings and []byte that already parse as JSON are stored
// verbatim, so callers can pass a raw response body straight through; anything
// else is marshalled. Empty / nil → nil so the column stays NULL.
func toJSONBString(v any) *string {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		if json.Valid([]byte(x)) {
			return &x
		}
		b, err := json.Marshal(x)
		if err != nil {
			return nil
		}
		s := string(b)
		return &s
	case []byte:
		if len(x) == 0 {
			return nil
		}
		if json.Valid(x) {
			s := string(x)
			return &s
		}
		b, err := json.Marshal(string(x))
		if err != nil {
			return nil
		}
		s := string(b)
		return &s
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		s := string(b)
		return &s
	}
}
