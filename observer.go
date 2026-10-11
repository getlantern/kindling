package kindling

import (
	"context"
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
	// OutcomeUnfinished means it hadn't finished connecting, or connected only
	// after another transport had won.
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
// fn should be quick. Transports in tiers that were never raced are left out.
func WithRaceObserver(fn func(ctx context.Context, race Race)) Option {
	return func(k *kindling) error {
		k.raceObserver = fn
		return nil
	}
}

// raceRecord collects one request's attempts. Only the goroutine running
// RoundTrip touches it.
type raceRecord struct {
	start    time.Time
	race     Race
	reported map[string]bool
	raced    []string
}

func newRaceRecord() *raceRecord {
	return &raceRecord{start: time.Now(), reported: make(map[string]bool)}
}

func (r *raceRecord) racing(tier []Transport) {
	for _, tr := range tier {
		r.raced = append(r.raced, tr.Name())
	}
}

func (r *raceRecord) add(name string, o Outcome, status int, err error) {
	r.reported[name] = true
	r.race.Attempts = append(r.race.Attempts, Attempt{
		Transport: name, Outcome: o, StatusCode: status, Err: err, Elapsed: time.Since(r.start),
	})
}

// won marks name's response as the one returned, upgrading a held fallback.
func (r *raceRecord) won(name string) {
	r.race.Winner = name
	for i := len(r.race.Attempts) - 1; i >= 0; i-- {
		if r.race.Attempts[i].Transport == name {
			r.race.Attempts[i].Outcome = OutcomeWon
			return
		}
	}
}

// finish adds the transports that never reported and returns the race.
func (r *raceRecord) finish() Race {
	for _, name := range r.raced {
		if !r.reported[name] {
			r.reported[name] = true
			r.race.Attempts = append(r.race.Attempts, Attempt{Transport: name, Outcome: OutcomeUnfinished, Elapsed: time.Since(r.start)})
		}
	}
	return r.race
}
