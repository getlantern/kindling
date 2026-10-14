package kindling

import (
	"context"
	"errors"
	"time"
)

// Outcome is what happened to one transport during a request.
type Outcome string

const (
	// OutcomeConnectFailed means the transport never connected.
	OutcomeConnectFailed Outcome = "connect_failed"
	// OutcomeRequestFailed means it connected but the request failed.
	OutcomeRequestFailed Outcome = "request_failed"
	// OutcomeRetryStatus means it returned a 5xx or 403 and another transport
	// was tried.
	OutcomeRetryStatus Outcome = "retry_status"
	// OutcomeWon means its response was the one returned.
	OutcomeWon Outcome = "won"
	// OutcomeUnfinished means it produced no request outcome before the request
	// ended: still connecting, connected but never chosen to send, or cut off
	// by the time budget.
	OutcomeUnfinished Outcome = "unfinished"
)

// Attempt is one transport's part in a request.
type Attempt struct {
	Transport string
	Outcome   Outcome
	// StatusCode is the response status, when a response arrived.
	StatusCode int
	// Err is why the transport failed, for the failed outcomes.
	Err error
	// Elapsed is the time from the start of the request to this outcome.
	Elapsed time.Duration
}

// Race reports how kindling handled one request.
type Race struct {
	// Winner is the transport whose response was returned, or "" if none was.
	Winner   string
	Attempts []Attempt
}

// WithRaceObserver calls fn after every request with how each transport fared,
// on the request's goroutine and with its context, before RoundTrip returns.
// fn should be quick. Transports that never joined the race are left out.
func WithRaceObserver(fn func(ctx context.Context, race Race)) Option {
	return func(k *kindling) error {
		k.raceObserver = fn
		return nil
	}
}

// raceRecord collects one request's attempts. Only the goroutine running
// RoundTrip touches it. A nil record, used when nobody observes, records nothing.
// Attempts are keyed by race position, since transport names needn't be unique.
type raceRecord struct {
	start    time.Time
	race     Race
	names    []string // by race position
	reported []bool   // by race position
	ids      []int    // race position of each entry in race.Attempts
}

func newRaceRecord() *raceRecord {
	return &raceRecord{start: time.Now()}
}

// racing registers transports joining the race and returns the race position
// of the first.
func (r *raceRecord) racing(tier []Transport) int {
	if r == nil {
		return 0
	}
	base := len(r.names)
	for _, tr := range tier {
		r.names = append(r.names, tr.Name())
		r.reported = append(r.reported, false)
	}
	return base
}

func (r *raceRecord) add(id int, o Outcome, status int, err error) {
	r.addAt(id, o, status, err, time.Now())
}

// addAt records an outcome that happened at a known time, which can be well
// before the race goroutine gets to it.
func (r *raceRecord) addAt(id int, o Outcome, status int, err error, at time.Time) {
	if r == nil || id < 0 || id >= len(r.names) {
		return
	}
	r.reported[id] = true
	r.ids = append(r.ids, id)
	r.race.Attempts = append(r.race.Attempts, Attempt{
		Transport: r.names[id], Outcome: o, StatusCode: status, Err: err, Elapsed: at.Sub(r.start),
	})
}

// connectFailed records a connection failure, unless the request's own budget
// ending caused it: that attempt was cut off, so it stays unfinished.
func (r *raceRecord) connectFailed(ctx context.Context, id int, err error, at time.Time) {
	if cause := ctx.Err(); cause != nil && errors.Is(err, cause) {
		return
	}
	r.addAt(id, OutcomeConnectFailed, 0, err, at)
}

// won marks the response from race position id as the one returned, upgrading
// a held fallback.
func (r *raceRecord) won(id int) {
	if r == nil || id < 0 || id >= len(r.names) {
		return
	}
	r.race.Winner = r.names[id]
	for i := len(r.ids) - 1; i >= 0; i-- {
		if r.ids[i] == id {
			r.race.Attempts[i].Outcome = OutcomeWon
			return
		}
	}
}

// finish adds the transports that never reported and returns the race.
func (r *raceRecord) finish() Race {
	for id, done := range r.reported {
		if !done {
			r.add(id, OutcomeUnfinished, 0, nil)
		}
	}
	return r.race
}
