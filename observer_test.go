package kindling

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type statusRT int

func (s statusRT) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: int(s), Body: io.NopCloser(strings.NewReader(""))}, nil
}

func returns(code int) func(context.Context, string) (http.RoundTripper, error) {
	return func(context.Context, string) (http.RoundTripper, error) { return statusRT(code), nil }
}

func failsToConnect(err error) func(context.Context, string) (http.RoundTripper, error) {
	return func(context.Context, string) (http.RoundTripper, error) { return nil, err }
}

func observe(t *testing.T, method string, transports ...Transport) (Race, *http.Response, error) {
	t.Helper()
	var got []Race
	rt := newRaceTransport("test", testLog, func(string) {}, transports)
	rt.observer = func(_ context.Context, r Race) { got = append(got, r) }
	req, err := http.NewRequest(method, "https://origin.example/v1/config", nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	require.Len(t, got, 1, "observer runs once per request")
	return got[0], resp, err
}

func byTransport(r Race) map[string]Attempt {
	m := make(map[string]Attempt, len(r.Attempts))
	for _, a := range r.Attempts {
		m[a.Transport] = a
	}
	return m
}

func TestRaceObserver_FallsBackToLaterTier(t *testing.T) {
	dialErr := errors.New("smart dial: blocked")
	race, resp, err := observe(t, http.MethodGet,
		&mockTransport{name: "domainfront", newRoundTripper: returns(http.StatusForbidden)},
		&mockTransport{name: "smart", newRoundTripper: failsToConnect(dialErr)},
		&mockTransport{name: "soar", priority: priorityLastResort, newRoundTripper: returns(http.StatusOK)},
	)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "soar", race.Winner)

	a := byTransport(race)
	require.Len(t, a, 3)
	assert.Equal(t, OutcomeRetryStatus, a["domainfront"].Outcome)
	assert.Equal(t, http.StatusForbidden, a["domainfront"].StatusCode)
	assert.Equal(t, OutcomeConnectFailed, a["smart"].Outcome)
	assert.ErrorIs(t, a["smart"].Err, dialErr)
	assert.Equal(t, OutcomeWon, a["soar"].Outcome)
	assert.Equal(t, http.StatusOK, a["soar"].StatusCode)
}

func TestRaceObserver_HeldFallbackWins(t *testing.T) {
	race, resp, err := observe(t, http.MethodGet,
		&mockTransport{name: "smart", newRoundTripper: returns(http.StatusForbidden)},
		&mockTransport{name: "soar", priority: priorityLastResort, newRoundTripper: failsToConnect(errors.New("no resolvers"))},
	)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "smart", race.Winner)
	a := byTransport(race)
	assert.Equal(t, OutcomeWon, a["smart"].Outcome)
	assert.Equal(t, http.StatusForbidden, a["smart"].StatusCode)
	assert.Equal(t, OutcomeConnectFailed, a["soar"].Outcome)
}

func TestRaceObserver_UnfinishedAndUnracedTiers(t *testing.T) {
	race, _, err := observe(t, http.MethodGet,
		&mockTransport{name: "fast", newRoundTripper: returns(http.StatusOK)},
		&mockTransport{name: "slow", newRoundTripper: func(ctx context.Context, _ string) (http.RoundTripper, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		&mockTransport{name: "soar", priority: priorityLastResort, newRoundTripper: returns(http.StatusOK)},
	)
	require.NoError(t, err)
	assert.Equal(t, "fast", race.Winner)
	a := byTransport(race)
	assert.Equal(t, OutcomeWon, a["fast"].Outcome)
	assert.Equal(t, OutcomeUnfinished, a["slow"].Outcome)
	_, raced := a["soar"]
	assert.False(t, raced, "a tier that never raced is left out")
}

func TestRaceObserver_NonIdempotentFailure(t *testing.T) {
	rtErr := errors.New("connection reset")
	race, _, err := observe(t, http.MethodPost,
		&mockTransport{name: "smart", newRoundTripper: func(context.Context, string) (http.RoundTripper, error) {
			return errorRoundTripper{err: rtErr}, nil
		}},
	)
	require.Error(t, err)
	assert.Empty(t, race.Winner)
	a := byTransport(race)
	assert.Equal(t, OutcomeRequestFailed, a["smart"].Outcome)
	assert.ErrorIs(t, a["smart"].Err, rtErr)
}

type slowRT struct {
	wait time.Duration
	code int
}

func (s slowRT) RoundTrip(*http.Request) (*http.Response, error) {
	time.Sleep(s.wait)
	return statusRT(s.code).RoundTrip(nil)
}

type respAndErrRT struct{}

func (respAndErrRT) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader(""))}, errors.New("stream reset")
}

// A failure that lands while the winner is sending its request is still
// reported as a failure, not unfinished.
func TestRaceObserver_FailureDuringWinnerRequest(t *testing.T) {
	race, _, err := observe(t, http.MethodGet,
		&mockTransport{name: "slow-request", newRoundTripper: func(context.Context, string) (http.RoundTripper, error) {
			return slowRT{wait: 100 * time.Millisecond, code: http.StatusOK}, nil
		}},
		&mockTransport{name: "late-fail", newRoundTripper: func(context.Context, string) (http.RoundTripper, error) {
			time.Sleep(20 * time.Millisecond)
			return nil, errors.New("blocked")
		}},
	)
	require.NoError(t, err)
	a := byTransport(race)
	assert.Equal(t, OutcomeWon, a["slow-request"].Outcome)
	assert.Equal(t, OutcomeConnectFailed, a["late-fail"].Outcome)
	assert.Less(t, a["late-fail"].Elapsed, 90*time.Millisecond, "timed when it failed, not when the winner finished")
}

func TestRaceObserver_ResponseWithErrorIsNotAWin(t *testing.T) {
	race, _, err := observe(t, http.MethodPost,
		&mockTransport{name: "smart", newRoundTripper: func(context.Context, string) (http.RoundTripper, error) {
			return respAndErrRT{}, nil
		}},
	)
	require.Error(t, err)
	assert.Empty(t, race.Winner)
	assert.Equal(t, OutcomeRequestFailed, byTransport(race)["smart"].Outcome)
}

func TestRaceObserver_DuplicateNames(t *testing.T) {
	race, _, err := observe(t, http.MethodGet,
		&mockTransport{name: "custom", newRoundTripper: returns(http.StatusOK)},
		&mockTransport{name: "custom", newRoundTripper: func(ctx context.Context, _ string) (http.RoundTripper, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
	)
	require.NoError(t, err)
	assert.Equal(t, "custom", race.Winner)
	var outcomes []Outcome
	for _, a := range race.Attempts {
		outcomes = append(outcomes, a.Outcome)
	}
	assert.ElementsMatch(t, []Outcome{OutcomeWon, OutcomeUnfinished}, outcomes, "each same-named transport gets its own attempt")
}

func TestRaceObserver_NilRecordIsInert(t *testing.T) {
	var r *raceRecord
	assert.Zero(t, r.racing([]Transport{&mockTransport{name: "x"}}))
	r.add(0, OutcomeWon, 200, nil)
	r.won(0)
}

// The public option reaches requests made through NewHTTPClient.
func TestWithRaceObserver(t *testing.T) {
	var got []Race
	k, err := NewKindling("test",
		WithTransport(&mockTransport{name: "custom", newRoundTripper: returns(http.StatusOK)}),
		WithRaceObserver(func(_ context.Context, r Race) { got = append(got, r) }),
	)
	require.NoError(t, err)
	resp, err := k.NewHTTPClient().Get("https://origin.example/v1/config")
	require.NoError(t, err)
	resp.Body.Close()
	require.Len(t, got, 1)
	assert.Equal(t, "custom", got[0].Winner)
}

// An attempt cut off by the request's budget is unfinished, not a failure;
// any other error is still a connection failure.
func TestRaceObserver_BudgetCutoffIsUnfinished(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := newRaceRecord()
	rec.racing([]Transport{&mockTransport{name: "cutoff"}, &mockTransport{name: "blocked"}})
	rec.connectFailed(ctx, 0, fmt.Errorf("dial: %w", ctx.Err()), time.Now())
	rec.connectFailed(ctx, 1, errors.New("connection refused"), time.Now())
	a := byTransport(rec.finish())
	assert.Equal(t, OutcomeUnfinished, a["cutoff"].Outcome)
	assert.Equal(t, OutcomeConnectFailed, a["blocked"].Outcome)
}

func TestWithTransport_RejectsEmptyName(t *testing.T) {
	_, err := NewKindling("test", WithTransport(&mockTransport{name: ""}))
	assert.Error(t, err)
}

// observeWithContext runs one request with ctx and returns what the observer saw.
func observeWithContext(t *testing.T, ctx context.Context, method string, transports ...Transport) (Race, error) {
	t.Helper()
	var got []Race
	rt := newRaceTransport("test", testLog, func(string) {}, transports)
	rt.observer = func(_ context.Context, r Race) { got = append(got, r) }
	req, err := http.NewRequestWithContext(ctx, method, "https://origin.example/v1/config", nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	require.Len(t, got, 1, "observer runs once per request")
	return got[0], err
}

// hangsUntilCanceled connects only once ctx ends, and then fails with its error.
func hangsUntilCanceled(ctx context.Context, _ string) (http.RoundTripper, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("dial: %w", ctx.Err())
}

// A caller that gives up while every transport is still connecting says
// nothing about the transports, so the race is canceled, not failed.
func TestRaceObserver_CallerCancelWhileConnecting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	race, err := observeWithContext(t, ctx, http.MethodGet,
		&mockTransport{name: "smart", newRoundTripper: hangsUntilCanceled},
		&mockTransport{name: "soar", startDelay: time.Hour, newRoundTripper: hangsUntilCanceled},
	)
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, race.Canceled)
	assert.Empty(t, race.Winner)
	a := byTransport(race)
	assert.Equal(t, OutcomeCanceled, a["smart"].Outcome)
	assert.NotContains(t, a, "soar", "a transport that never joined stays out")
}

// A POST is sent once, so the request the caller cancels is the race's
// final answer. It failed because of the caller, not the transport.
func TestRaceObserver_CallerCancelDuringRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sending := make(chan struct{})
	go func() {
		<-sending
		cancel()
	}()
	race, err := observeWithContext(t, ctx, http.MethodPost,
		&mockTransport{name: "smart", newRoundTripper: func(context.Context, string) (http.RoundTripper, error) {
			return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				close(sending)
				<-req.Context().Done()
				return nil, req.Context().Err()
			}), nil
		}},
	)
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, race.Canceled)
	assert.Equal(t, OutcomeCanceled, byTransport(race)["smart"].Outcome)
}

// The caller's own deadline ends the request just as cancelling does.
func TestRaceObserver_CallerDeadlineIsCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	race, err := observeWithContext(t, ctx, http.MethodGet,
		&mockTransport{name: "smart", newRoundTripper: hangsUntilCanceled},
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, race.Canceled)
	assert.Equal(t, OutcomeCanceled, byTransport(race)["smart"].Outcome)
}

// A request that failed on its own isn't canceled, even if the caller's
// context ends before the race reports.
func TestRaceObserver_FailureBeforeCancelIsAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := newRaceRecord()
	rec.raceEnded(ctx)
	rec.racing([]Transport{&mockTransport{name: "smart"}})
	rec.requestFailed(0, 0, errors.New("connection reset"), false)
	race := rec.finish()
	assert.False(t, race.Canceled)
	assert.Equal(t, OutcomeRequestFailed, byTransport(race)["smart"].Outcome)
}

// A caller can cancel with its own cause, which transports may return in
// place of context.Canceled.
func TestRaceObserver_CallerCancelWithCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	stop := errors.New("refresh superseded this fetch")
	sending := make(chan struct{})
	go func() {
		<-sending
		cancel(stop)
	}()
	race, err := observeWithContext(t, ctx, http.MethodPost,
		&mockTransport{name: "smart", newRoundTripper: func(context.Context, string) (http.RoundTripper, error) {
			return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				close(sending)
				<-req.Context().Done()
				return nil, context.Cause(req.Context())
			}), nil
		}},
	)
	// The race may see its own context end first and return that instead.
	require.Error(t, err)
	assert.True(t, race.Canceled)
	assert.Equal(t, OutcomeCanceled, byTransport(race)["smart"].Outcome)
}

// Whether a request was canceled is judged when it returns: a transport's own
// timeout before the caller's deadline is a failure, even if the caller's
// deadline has passed by the time the race settles it.
func TestEndedBy(t *testing.T) {
	live := context.Background()
	assert.False(t, endedBy(live, context.DeadlineExceeded), "a transport's own timeout")

	ended, cancel := context.WithCancel(live)
	cancel()
	assert.True(t, endedBy(ended, fmt.Errorf("send: %w", context.Canceled)))
	assert.False(t, endedBy(ended, errors.New("connection reset")))
	assert.False(t, endedBy(ended, nil))

	stop := errors.New("superseded")
	withCause, cancelCause := context.WithCancelCause(live)
	cancelCause(stop)
	assert.True(t, endedBy(withCause, fmt.Errorf("send: %w", stop)))
}

// The budget ending the race decides the outcome, even if the caller gives up
// before the race is reported.
func TestRaceObserver_BudgetEndBeatsLaterCancel(t *testing.T) {
	caller, cancel := context.WithCancel(context.Background())
	budget, stop := context.WithTimeoutCause(caller, time.Nanosecond, errBudgetSpent)
	defer stop()
	<-budget.Done()
	cancel()

	rec := newRaceRecord()
	rec.racing([]Transport{&mockTransport{name: "smart"}})
	rec.raceEnded(budget)
	race := rec.finish()
	assert.False(t, race.Canceled)
	assert.Equal(t, OutcomeUnfinished, byTransport(race)["smart"].Outcome)

	caller, cancel = context.WithCancel(context.Background())
	budget, stop = context.WithTimeoutCause(caller, time.Hour, errBudgetSpent)
	defer stop()
	cancel()
	rec = newRaceRecord()
	rec.racing([]Transport{&mockTransport{name: "smart"}})
	rec.raceEnded(budget)
	race = rec.finish()
	assert.True(t, race.Canceled, "the caller ended it first")
	assert.Equal(t, OutcomeCanceled, byTransport(race)["smart"].Outcome)
}

// Transports may fail with the context's cause rather than its error. Either
// way the attempt was cut off, not failed: unfinished when the budget ran out,
// canceled when the caller ended the request.
func TestRaceObserver_ConnectFailureWithCause(t *testing.T) {
	budget, stop := context.WithTimeoutCause(context.Background(), time.Nanosecond, errBudgetSpent)
	defer stop()
	<-budget.Done()
	rec := newRaceRecord()
	rec.racing([]Transport{&mockTransport{name: "smart"}})
	rec.connectFailed(budget, 0, fmt.Errorf("dial: %w", context.Cause(budget)), time.Now())
	rec.raceEnded(budget)
	assert.Equal(t, OutcomeUnfinished, byTransport(rec.finish())["smart"].Outcome)

	superseded := errors.New("superseded")
	caller, cancel := context.WithCancelCause(context.Background())
	budget, stop = context.WithTimeoutCause(caller, time.Hour, errBudgetSpent)
	defer stop()
	cancel(superseded)
	rec = newRaceRecord()
	rec.racing([]Transport{&mockTransport{name: "smart"}})
	rec.connectFailed(budget, 0, fmt.Errorf("dial: %w", superseded), time.Now())
	rec.raceEnded(budget)
	race := rec.finish()
	assert.True(t, race.Canceled)
	assert.Equal(t, OutcomeCanceled, byTransport(race)["smart"].Outcome)
}

// A request the caller cancels after the budget ran out was cut off by the
// budget, even if its result reaches the race before the timeout does.
func TestBudgetSpent(t *testing.T) {
	caller, cancel := context.WithCancel(context.Background())
	budget, stop := context.WithTimeoutCause(caller, time.Nanosecond, errBudgetSpent)
	defer stop()
	<-budget.Done()
	cancel()
	assert.True(t, endedBy(caller, context.Canceled))
	assert.True(t, budgetSpent(budget), "the budget ended it first")

	caller, cancel = context.WithCancel(context.Background())
	budget, stop = context.WithTimeoutCause(caller, time.Hour, errBudgetSpent)
	defer stop()
	cancel()
	assert.False(t, budgetSpent(budget), "the caller ended it first")
	assert.False(t, budgetSpent(context.Background()))
}
