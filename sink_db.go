// sink_db.go
//
// Records fan out to a buffered channel, drained by a background goroutine
// that bulk-inserts. Flush triggers: the batch hits batchSize, flushInterval
// elapses, or the sink is closed.

package logger

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type dbSink struct {
	pool          *pgxpool.Pool
	queue         chan Entry
	batchSize     int
	flushInterval time.Duration
	insertTimeout time.Duration

	wg       sync.WaitGroup
	stopOnce sync.Once
}

func newDBSink(pool *pgxpool.Pool, s LoggerSetup) *dbSink {
	d := &dbSink{
		pool:          pool,
		queue:         make(chan Entry, s.QueueSize),
		batchSize:     s.BatchSize,
		flushInterval: s.FlushInterval,
		insertTimeout: s.InsertTimeout,
	}
	d.wg.Add(1)
	go d.run()
	return d
}

func (s *dbSink) enqueue(e Entry) {
	select {
	case s.queue <- e:
	default:
		// Queue full: drop. Logging must never block the app. If you're
		// dropping often, raise QueueSize or shorten FlushInterval.
	}
}

func (s *dbSink) stop() {
	s.stopOnce.Do(func() {
		close(s.queue)
		s.wg.Wait()
	})
}

func (s *dbSink) run() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	batch := make([]Entry, 0, s.batchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.insertBatch(batch)
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-s.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= s.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

const insertColumns = 12

// insertBatch writes one multi-row INSERT.
//
// id is omitted deliberately: the column is uuid NOT NULL, so it's expected to
// carry a DB-side default (gen_random_uuid()). If it doesn't, every insert
// fails on a null id and the sink will say so on stderr.
func (s *dbSink) insertBatch(batch []Entry) {
	var sb strings.Builder
	sb.WriteString(`INSERT INTO logs (created_at, level, application, message, user_id, username, process_id, trace, race_details, bet, request, response) VALUES `)

	args := make([]any, 0, len(batch)*insertColumns)
	for i, e := range batch {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i*insertColumns + 1
		sb.WriteByte('(')
		for c := 0; c < insertColumns; c++ {
			if c > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "$%d", base+c)
		}
		sb.WriteByte(')')

		args = append(args,
			e.Time, e.LogType, e.Application, dbMessage(e),
			e.UserID, e.Username, e.ProcessID,
			e.Trace, raceDetailsJSONB(e.RaceDetails), e.Bet, e.Request, e.Response,
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.insertTimeout)
	defer cancel()

	if _, err := s.pool.Exec(ctx, sb.String(), args...); err != nil {
		// A whole batch is lost here, so the count matters more than the rows.
		// Deliberately raw stderr, not this package's logger: an Error here
		// would fan straight back into this sink and fail the same way.
		fmt.Fprintf(os.Stderr, "logger: batch insert failed (%d rows dropped): %v\n", len(batch), err)
	}
}
