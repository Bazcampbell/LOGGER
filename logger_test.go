package logger

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func decodeJSONB(t *testing.T, label string, s *string) map[string]any {
	t.Helper()
	if s == nil {
		t.Fatalf("%s: expected JSONB, got NULL", label)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(*s), &m); err != nil {
		t.Fatalf("%s: not valid JSON (%v): %s", label, err, *s)
	}
	return m
}

func TestInfoLogFillsColumns(t *testing.T) {
	e := newEntry(InfoLog{
		UserID:    "u1",
		Username:  "baz",
		ProcessID: "p1",
		Message:   "first race seen",
		RaceDetails: &RaceDetails{
			Venue:      "ASCOT",
			RaceNumber: 4,
		},
	}, slog.LevelInfo, time.Now(), 0, "pegasus", "admin")

	if e.UserID != "u1" || e.Username != "baz" || e.ProcessID != "p1" {
		t.Errorf("identity columns wrong: %+v", e)
	}
	if e.LogType != "INFO" || e.Application != "pegasus" {
		t.Errorf("logType/application wrong: %+v", e)
	}

	race := decodeJSONB(t, "race_details", raceDetailsJSONB(e.RaceDetails))
	if race["venue"] != "ASCOT" || race["race_number"] != float64(4) {
		t.Errorf("race_details = %v", race)
	}
	// Unset runner fields must not appear.
	if _, ok := race["runner_number"]; ok {
		t.Errorf("zero runner_number should be omitted: %v", race)
	}

	if e.Bet != nil {
		t.Errorf("InfoLog should not populate bet_details: %v", *e.Bet)
	}
}

func TestNilRaceDetailsIsNull(t *testing.T) {
	// Program-level logs have no race context; the column must be NULL rather
	// than "{}" so `WHERE race_details IS NULL` finds them.
	e := newEntry(InfoLog{Message: "server started"}, slog.LevelInfo, time.Now(), 0, "pegasus", "")
	if got := raceDetailsJSONB(e.RaceDetails); got != nil {
		t.Errorf("race_details = %v, want NULL", *got)
	}

	// An entirely empty RaceDetails is also NULL rather than an empty object.
	e2 := newEntry(InfoLog{RaceDetails: &RaceDetails{}}, slog.LevelInfo, time.Now(), 0, "pegasus", "")
	if got := raceDetailsJSONB(e2.RaceDetails); got != nil {
		t.Errorf("empty RaceDetails should yield NULL, got %v", *got)
	}
}

// TestErrorFoldsIntoMessage pins the workaround for the logs table having no
// error column: the error text is appended to the message on the way to the
// DB, so it can't be lost when a caller sets both.
func TestErrorFoldsIntoMessage(t *testing.T) {
	both := newEntry(ErrorLog{
		Message: "unable to send bet notification",
		Error:   errors.New("connection refused"),
	}, slog.LevelError, time.Now(), 0, "davo", "")

	if got := dbMessage(both); got != "unable to send bet notification: connection refused" {
		t.Errorf("dbMessage = %q", got)
	}
	// The Telegram card keeps them apart, so it must not see a merged string.
	if both.Message != "unable to send bet notification" {
		t.Errorf("Entry.Message should stay unmerged, got %q", both.Message)
	}

	// Error only — Message already falls back to it, so it must not double up.
	errOnly := newEntry(ErrorLog{Error: errors.New("boom")}, slog.LevelError, time.Now(), 0, "davo", "")
	if got := dbMessage(errOnly); got != "boom" {
		t.Errorf("dbMessage = %q, want no duplication", got)
	}

	// Message only.
	msgOnly := newEntry(ErrorLog{Message: "reconnecting"}, slog.LevelWarn, time.Now(), 0, "davo", "")
	if got := dbMessage(msgOnly); got != "reconnecting" {
		t.Errorf("dbMessage = %q", got)
	}
}

func TestErrorLogCarriesErrorAndPayloads(t *testing.T) {
	e := newEntry(ErrorLog{
		UserID:   "u1",
		Message:  "unable to send bet notification",
		Error:    errors.New("connection refused"),
		Request:  `{"a":1}`,
		Response: map[string]any{"b": 2},
	}, slog.LevelError, time.Now(), 0, "davo", "")

	if e.Error != "connection refused" {
		t.Errorf("Error = %q", e.Error)
	}
	if e.Message != "unable to send bet notification" {
		t.Errorf("Message = %q", e.Message)
	}
	if e.Request == nil || *e.Request != `{"a":1}` {
		t.Errorf("request should be stored verbatim when already JSON: %v", e.Request)
	}
	if e.Response == nil || *e.Response != `{"b":2}` {
		t.Errorf("response = %v", e.Response)
	}
}

func TestErrorLogMessageFallsBackToError(t *testing.T) {
	e := newEntry(ErrorLog{Error: errors.New("boom")}, slog.LevelError, time.Now(), 0, "davo", "")
	if e.Message != "boom" {
		t.Errorf("Message = %q, want the error text", e.Message)
	}

	// A nil Error must not panic or produce a spurious error string.
	e2 := newEntry(ErrorLog{Message: "just a warning"}, slog.LevelWarn, time.Now(), 0, "davo", "")
	if e2.Error != "" {
		t.Errorf("Error = %q, want empty", e2.Error)
	}
}

// TestErrorLogTraceIsAdditive pins the decision that a caller-supplied Trace
// augments the captured call site rather than replacing it.
func TestErrorLogTraceIsAdditive(t *testing.T) {
	stack := "goroutine 1 [running]:\nmain.main()"

	sink := &captureSink{}
	newCapturing(sink).Error(ErrorLog{Message: "panic recovered", Trace: &stack})

	tr := decodeJSONB(t, "trace", sink.one(t).Trace)

	if tr["trace"] != stack {
		t.Errorf("caller trace missing: %v", tr)
	}
	if !strings.Contains(tr["file"].(string), "logger_test.go") {
		t.Errorf("call site was lost: %v", tr)
	}
}

func TestBetLogSplitsRaceAndBetDetails(t *testing.T) {
	e := newEntry(BetLog{
		UserID: "u1",
		RaceDetails: &RaceDetails{
			Venue:        "ROCKHAMPTON",
			RaceNumber:   7,
			RunnerNumber: 3,
			RunnerName:   "Brank Daddy",
		},
		Market:    "Fixed Win",
		Endpoint:  "betmatic",
		TargetLia: 240,
		Message:   "bet placed",
	}, LevelBet, time.Now(), 0, "pegasus", "")

	race := decodeJSONB(t, "race_details", raceDetailsJSONB(e.RaceDetails))
	for k, want := range map[string]any{
		"venue":         "ROCKHAMPTON",
		"race_number":   float64(7),
		"runner_number": float64(3),
		"runner_name":   "Brank Daddy",
	} {
		if race[k] != want {
			t.Errorf("race_details[%q] = %v, want %v", k, race[k], want)
		}
	}

	bet := decodeJSONB(t, "bet_details", e.Bet)
	for k, want := range map[string]any{
		"market":     "Fixed Win",
		"endpoint":   "betmatic",
		"target_lia": float64(240),
	} {
		if bet[k] != want {
			t.Errorf("bet_details[%q] = %v, want %v", k, bet[k], want)
		}
	}
	if _, ok := bet["placed_at"]; !ok {
		t.Error("bet_details should carry placed_at")
	}
	// Race context must not be duplicated into bet_details.
	if _, ok := bet["venue"]; ok {
		t.Errorf("venue should live in race_details only: %v", bet)
	}
	// Zero-valued fields stay out.
	if _, ok := bet["stake"]; ok {
		t.Error("zero stake should be omitted from bet_details")
	}
}

func TestTelegramCardRendersBetFields(t *testing.T) {
	e := newEntry(BetLog{
		RaceDetails: &RaceDetails{
			Venue:        "ASCOT",
			RaceNumber:   3,
			RunnerNumber: 5,
			RunnerName:   "Brank Daddy",
		},
		Market:   "Fixed Win",
		Stake:    10.5,
		Odds:     4.2,
		Username: "baz",
		Message:  "bet placed",
	}, LevelBet, time.Now(), 0, "pegasus", "")

	out := formatTelegram(e)
	for _, want := range []string{"ASCOT R3", "Fixed Win", "#5", "Brank Daddy", "$10.50", "4.20", "baz"} {
		if !strings.Contains(out, want) {
			t.Errorf("telegram card missing %q:\n%s", want, out)
		}
	}
}

// Race context renders on non-bet logs too — an ERROR during a race should
// still say which race.
func TestTelegramCardRendersRaceOnErrorLog(t *testing.T) {
	e := newEntry(ErrorLog{
		Message:     "unable to send bet notification",
		Error:       errors.New("connection refused"),
		RaceDetails: &RaceDetails{Venue: "ASCOT", RaceNumber: 3},
	}, slog.LevelError, time.Now(), 0, "davo", "")

	out := formatTelegram(e)
	for _, want := range []string{"ASCOT R3", "connection refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("telegram card missing %q:\n%s", want, out)
		}
	}
}

func TestDefaultUserIDFallback(t *testing.T) {
	e := newEntry(InfoLog{Message: "no user"}, slog.LevelInfo, time.Now(), 0, "pegasus", "admin-id")
	if e.UserID != "admin-id" {
		t.Errorf("UserID = %q, want fallback", e.UserID)
	}

	e2 := newEntry(InfoLog{UserID: "explicit"}, slog.LevelInfo, time.Now(), 0, "pegasus", "admin-id")
	if e2.UserID != "explicit" {
		t.Errorf("explicit UserID should win, got %q", e2.UserID)
	}
}

// TestCallerSourceIsCallSite guards the runtime.Callers skip depth. If the
// wrapper chain ever grows a frame, every log line's source silently starts
// pointing at logger.go instead of the code that logged.
func TestCallerSourceIsCallSite(t *testing.T) {
	check := func(label string, e Entry) {
		t.Helper()
		tr := decodeJSONB(t, label+" trace", e.Trace)

		file, _ := tr["file"].(string)
		fn, _ := tr["function"].(string)

		if !strings.Contains(file, "logger_test.go") {
			t.Errorf("%s: source file = %q, want logger_test.go — skip depth is wrong", label, file)
		}
		if !strings.Contains(fn, "TestCallerSourceIsCallSite") {
			t.Errorf("%s: source function = %q, want the test function", label, fn)
		}
	}

	// Package-level path.
	sink := &captureSink{}
	prev := std.Swap(newCapturing(sink))
	Error(ErrorLog{Message: "via package function"})
	std.Store(prev)
	check("package-level", sink.one(t))

	// Instance path.
	sink2 := &captureSink{}
	newCapturing(sink2).Warn(ErrorLog{Message: "via method"})
	check("instance", sink2.one(t))
}

func TestLevelGatingSkipsSinks(t *testing.T) {
	t.Setenv("TESTAPP_LOG_LEVEL", "warn")

	sink := &captureSink{}
	l := newCapturing(sink)
	l.slog = slog.New(newTextHandler(resolveLevel("testapp")))

	l.Debug(InfoLog{Message: "chatty"})
	l.Info(InfoLog{Message: "routine"})
	l.Warn(ErrorLog{Message: "problem"})

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("expected only the WARN to reach the sink, got %d: %+v", len(got), got)
	}
	if got[0].LogType != "WARN" {
		t.Errorf("LogType = %q, want WARN", got[0].LogType)
	}
}

func TestChannelRouting(t *testing.T) {
	info := int64(1)
	fallback := int64(9)
	setup := &TelegramSetup{
		BotToken:         "x",
		InfoLogChannelID: &info,
		DefaultChannelID: &fallback,
	}

	cases := map[string]*int64{
		"INFO":  &info,
		"WARN":  &fallback, // no specific channel -> default
		"ERROR": &fallback,
		"BET":   &fallback,
		"DEBUG": nil, // never sent
	}

	for logType, want := range cases {
		got := setup.channelFor(logType)
		switch {
		case want == nil && got != nil:
			t.Errorf("%s routed to %d, want dropped", logType, *got)
		case want != nil && got == nil:
			t.Errorf("%s dropped, want channel %d", logType, *want)
		case want != nil && got != nil && *got != *want:
			t.Errorf("%s routed to %d, want %d", logType, *got, *want)
		}
	}

	if (&TelegramSetup{BotToken: "x"}).enabled() {
		t.Error("setup with no channels should report disabled")
	}
	if (&TelegramSetup{DefaultChannelID: &info}).enabled() {
		t.Error("setup with no bot token should report disabled")
	}
	var nilSetup *TelegramSetup
	if nilSetup.enabled() {
		t.Error("nil setup should report disabled")
	}
}

func TestTelegramSinkDedupesAndSummarises(t *testing.T) {
	ch := int64(42)
	fake := &fakeSender{}

	sink := newTGSink(fake, &TelegramSetup{
		BotToken:          "x",
		ErrorLogChannelID: &ch,
	}, LoggerSetup{TelegramQueueSize: 100, DedupeWindow: 150 * time.Millisecond}.withDefaults())

	e := Entry{LogType: "ERROR", Application: "pegasus", Message: "boom", Error: "refused"}
	for i := 0; i < 5; i++ {
		sink.enqueue(e)
	}

	// Shutdown force-sweeps, so the summary is emitted deterministically.
	sink.stop()

	sent := fake.all()
	if len(sent) != 2 {
		t.Fatalf("expected 1 message + 1 summary, got %d: %v", len(sent), sent)
	}
	if !strings.Contains(sent[0], "boom") {
		t.Errorf("first message should be the log itself: %q", sent[0])
	}
	if !strings.Contains(sent[1], "4 more") {
		t.Errorf("summary should count the 4 swallowed repeats: %q", sent[1])
	}
}

func TestTelegramSinkDropsUnroutedLevels(t *testing.T) {
	ch := int64(42)
	fake := &fakeSender{}
	sink := newTGSink(fake, &TelegramSetup{
		BotToken:          "x",
		ErrorLogChannelID: &ch,
	}, LoggerSetup{}.withDefaults())

	sink.enqueue(Entry{LogType: "DEBUG", Message: "chatty"})
	sink.enqueue(Entry{LogType: "INFO", Message: "routine"})
	sink.stop()

	if sent := fake.all(); len(sent) != 0 {
		t.Errorf("levels with no channel should never be sent, got %v", sent)
	}
}

func TestRateLimiterPacesPerChannel(t *testing.T) {
	r := newRateLimiter()

	var slept time.Duration
	now := time.Now()
	r.now = func() time.Time { return now }
	r.sleep = func(d time.Duration) {
		slept += d
		now = now.Add(d)
	}

	for i := 0; i < int(chatBurst); i++ {
		r.wait(1)
	}
	if slept != 0 {
		t.Errorf("burst should not sleep, slept %v", slept)
	}

	r.wait(1)
	if slept <= 0 {
		t.Error("expected the post-burst send to be paced")
	}

	before := slept
	r.wait(2)
	if slept != before {
		t.Errorf("second channel should have its own budget, slept extra %v", slept-before)
	}
}

func TestRateLimiterHonoursRetryAfter(t *testing.T) {
	r := newRateLimiter()
	now := time.Now()
	var slept time.Duration
	r.now = func() time.Time { return now }
	r.sleep = func(d time.Duration) {
		slept += d
		now = now.Add(d)
	}

	r.penalise(7, 30*time.Second)
	r.wait(7)

	if slept < 30*time.Second {
		t.Errorf("slept %v, want at least the 30s retry_after", slept)
	}
}

func TestEscapingPreventsMarkupInjection(t *testing.T) {
	e := Entry{
		LogType: "ERROR",
		Message: "bad <b>thing</b> & worse",
		Error:   "<script>alert(1)</script>",
	}
	out := formatTelegram(e)

	if strings.Contains(out, "<b>thing</b>") {
		t.Errorf("message markup was not escaped:\n%s", out)
	}
	if strings.Contains(out, "<script>") {
		t.Errorf("error markup was not escaped:\n%s", out)
	}
	if !strings.Contains(out, "&amp;") {
		t.Errorf("ampersand was not escaped:\n%s", out)
	}
}

func TestSplitFuncName(t *testing.T) {
	cases := []struct{ in, pkg, fn string }{
		{"pegasus_betting/packages/engine.(*Engine).AddProcess", "pegasus_betting/packages/engine", "(*Engine).AddProcess"},
		{"main.main", "main", "main"},
		{"main", "main", ""},
	}
	for _, c := range cases {
		pkg, fn := splitFuncName(c.in)
		if pkg != c.pkg || fn != c.fn {
			t.Errorf("splitFuncName(%q) = (%q, %q), want (%q, %q)", c.in, pkg, fn, c.pkg, c.fn)
		}
	}
}

// --- helpers ---

// captureSink stands in for the DB sink so tests can assert on the Entry that
// would have been persisted.
type captureSink struct {
	mu      sync.Mutex
	entries []Entry
}

func (c *captureSink) enqueue(e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
}

func (c *captureSink) stop() {}

func (c *captureSink) all() []Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Entry(nil), c.entries...)
}

func (c *captureSink) one(t *testing.T) Entry {
	t.Helper()
	all := c.all()
	if len(all) != 1 {
		t.Fatalf("expected exactly 1 entry, got %d", len(all))
	}
	return all[0]
}

// newCapturing builds a Logger that runs the real emit path but diverts
// entries to sink instead of Postgres/Telegram.
func newCapturing(sink *captureSink) *Logger {
	return &Logger{
		slog:        slog.New(newTextHandler(slog.LevelDebug)),
		application: "test",
		db:          sink,
	}
}

type fakeSender struct {
	mu   sync.Mutex
	sent []string
}

func (f *fakeSender) SendLog(text string, channelID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}

func (f *fakeSender) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}
