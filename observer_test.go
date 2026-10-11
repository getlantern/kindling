package kindling

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

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
