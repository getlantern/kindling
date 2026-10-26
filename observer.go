package kindling

import (
	"context"
	"errors"
	"slices"
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
	// OutcomeCanceled means the caller ended the request, by cancelling its
	// context or letting its deadline pass, before this transport produced an
	// outcome or while it was failing because of that.
	OutcomeCanceled Outcome = "canceled"
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
	Winner string
	// Canceled reports that the caller ended the request before any response
	// was returned, cutting off at least one attempt (see OutcomeCanceled).
	// Such a request says little about the transports, so it shouldn't count
	// as a failure.
	Canceled bool
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
	// callerEnded is whether the caller had ended the request when the race
	// ended (see raceEnded).
	callerEnded bool
	start       time.Time
	race        Race
	names       []string // by race position
	reported    []bool   // by race position
	ids         []int    // race position of each entry in race.Attempts
}

func newRaceRecord() *raceRecord {
	return &raceRecord{start: time.Now()}
}

// errBudgetSpent is the cause of a request's context ending when kindling's
// time budget for it runs out, as opposed to the caller ending it.
var errBudgetSpent = errors.New("kindling: request time budget spent")

// raceEnded notes whether the caller had ended the request when the race
// ended. ctx is the race's context, whose cause is errBudgetSpent if the
// budget ended it first, so a caller giving up later doesn't count.
func (r *raceRecord) raceEnded(ctx context.Context) {
	if r == nil || ctx.Err() == nil {
		return
	}
	r.callerEnded = !budgetSpent(ctx)
}

// budgetSpent reports whether kindling's time budget ended ctx, the race's
// context, before anything else did.
func budgetSpent(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errBudgetSpent)
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
	if endedBy(ctx, err) {
		return
	}
	r.addAt(id, OutcomeConnectFailed, 0, err, at)
}

// requestFailed records a failed request, as canceled if the caller ending
// the request caused it (see endedBy).
func (r *raceRecord) requestFailed(id int, status int, err error, canceled bool) {
	o := OutcomeRequestFailed
	if canceled {
		o = OutcomeCanceled
	}
	r.add(id, o, status, err)
}

// endedBy reports whether err is ctx ending: its error, or the cause it was
// cancelled with, which transports may return instead.
func endedBy(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() == nil {
		return false
	}
	return errors.Is(err, ctx.Err()) || errors.Is(err, context.Cause(ctx))
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

// finish adds the transports that never reported and returns the race. If
// the caller ended the request before a response was returned, they were cut
// off by it and are canceled rather than unfinished.
func (r *raceRecord) finish() Race {
	pending := OutcomeUnfinished
	if r.race.Winner == "" && r.callerEnded {
		pending = OutcomeCanceled
	}
	for id, done := range r.reported {
		if !done {
			r.add(id, pending, 0, nil)
		}
	}
	// A race whose transports had all failed before the caller gave up is
	// still a failure.
	r.race.Canceled = r.race.Winner == "" && slices.ContainsFunc(r.race.Attempts, func(a Attempt) bool {
		return a.Outcome == OutcomeCanceled
	})
	return r.race
}
