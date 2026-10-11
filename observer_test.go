package kindling

import (
	"context"
	"errors"
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
