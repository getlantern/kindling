package kindling

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hangs connects only when the request's budget ends, like a transport on a
// network that drops its packets.
func hangs(ctx context.Context, _ string) (http.RoundTripper, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// after connects after d and returns code.
func after(d time.Duration, code int) func(context.Context, string) (http.RoundTripper, error) {
	return func(ctx context.Context, _ string) (http.RoundTripper, error) {
		select {
		case <-time.After(d):
			return statusRT(code), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// counted wraps fn to record when it was first called, relative to start.
func counted(start *time.Time, at *atomic.Int64, fn func(context.Context, string) (http.RoundTripper, error)) func(context.Context, string) (http.RoundTripper, error) {
	return func(ctx context.Context, addr string) (http.RoundTripper, error) {
		at.Store(int64(time.Since(*start)))
		return fn(ctx, addr)
	}
}

func roundTrip(t *testing.T, rt *raceTransport, method string) (*http.Response, error, time.Duration) {
	t.Helper()
	req, err := http.NewRequest(method, "https://origin.example/v1/config", nil)
	require.NoError(t, err)
	begin := time.Now()
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	return resp, err, time.Since(begin)
}

// The case the delay exists for: the fast transports hang instead of failing,
// so waiting for them to fail would spend the request's whole budget. The
// delayed transport must join once its delay passes and win.
func TestStartDelay_JoinsWhileOthersHang(t *testing.T) {
	t.Parallel()
	var start time.Time
	var soarAt atomic.Int64
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "smart", newRoundTripper: hangs},
		&mockTransport{name: "domainfront", newRoundTripper: hangs},
		&mockTransport{name: "soar", priority: priorityLastResort, startDelay: 100 * time.Millisecond,
			newRoundTripper: counted(&start, &soarAt, returns(http.StatusOK))},
	})
	start = time.Now()
	resp, err, took := roundTrip(t, rt, http.MethodGet)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.GreaterOrEqual(t, time.Duration(soarAt.Load()), 100*time.Millisecond, "soar started before its delay")
	assert.Less(t, took, 5*time.Second, "soar should win long before the 80 s budget")
}

// A race the fast transports win before the delay never dials the delayed
// transport.
func TestStartDelay_NotDialedWhenOthersWinFirst(t *testing.T) {
	t.Parallel()
	var start time.Time
	var soarAt atomic.Int64
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "smart", newRoundTripper: after(20*time.Millisecond, http.StatusOK)},
		&mockTransport{name: "soar", priority: priorityLastResort, startDelay: 200 * time.Millisecond,
			newRoundTripper: counted(&start, &soarAt, returns(http.StatusOK))},
	})
	start = time.Now()
	_, err, _ := roundTrip(t, rt, http.MethodGet)
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	assert.Zero(t, soarAt.Load(), "soar was dialed though smart won before its delay")
}

// When every running transport fails outright, the delayed one starts at once
// rather than waiting out its delay.
func TestStartDelay_StartsAtOnceWhenOthersFail(t *testing.T) {
	t.Parallel()
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "smart", newRoundTripper: failsToConnect(errors.New("reset"))},
		&mockTransport{name: "domainfront", newRoundTripper: returns(http.StatusBadGateway)},
		&mockTransport{name: "soar", priority: priorityLastResort, startDelay: time.Minute,
			newRoundTripper: returns(http.StatusOK)},
	})
	resp, err, took := roundTrip(t, rt, http.MethodGet)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Less(t, took, 5*time.Second, "soar waited out its delay after the others had failed")
}

// Transports already racing keep going once the delayed one joins, so a slow
// fast transport still beats a delayed one that hangs.
func TestStartDelay_EarlierTransportsKeepRacing(t *testing.T) {
	t.Parallel()
	var got []Race
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "smart", newRoundTripper: after(200*time.Millisecond, http.StatusOK)},
		&mockTransport{name: "soar", priority: priorityLastResort, startDelay: 50 * time.Millisecond, newRoundTripper: hangs},
	})
	rt.observer = func(_ context.Context, r Race) { got = append(got, r) }
	resp, err, _ := roundTrip(t, rt, http.MethodGet)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, got, 1)
	assert.Equal(t, "smart", got[0].Winner)
	require.Len(t, got[0].Attempts, 2, "soar joined the race, so it's reported")
	assert.Equal(t, OutcomeUnfinished, got[0].Attempts[1].Outcome)
}

// A default-tier transport can be staggered too: it waits for its delay while
// its tier-mates race, and a later tier with no delay still waits for all of
// them to fail.
func TestStartDelay_StaggersDefaultTier(t *testing.T) {
	t.Parallel()
	var start time.Time
	var ampAt, lastAt atomic.Int64
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "smart", newRoundTripper: after(150*time.Millisecond, http.StatusBadGateway)},
		&mockTransport{name: "amp", startDelay: 50 * time.Millisecond,
			newRoundTripper: counted(&start, &ampAt, after(150*time.Millisecond, http.StatusBadGateway))},
		&mockTransport{name: "last", priority: priorityLastResort,
			newRoundTripper: counted(&start, &lastAt, returns(http.StatusOK))},
	})
	start = time.Now()
	resp, err, _ := roundTrip(t, rt, http.MethodGet)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.GreaterOrEqual(t, time.Duration(ampAt.Load()), 50*time.Millisecond, "amp started before its delay")
	assert.GreaterOrEqual(t, time.Duration(lastAt.Load()), 200*time.Millisecond,
		"the last tier has no delay, so it waits for amp to fail too")
}

// With only a delayed transport eligible, nothing else can fail first, so it
// starts at once.
func TestStartDelay_OnlyDelayedTransportEligible(t *testing.T) {
	t.Parallel()
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "soar", priority: priorityLastResort, startDelay: time.Minute, newRoundTripper: returns(http.StatusOK)},
	})
	resp, err, took := roundTrip(t, rt, http.MethodGet)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Less(t, took, 5*time.Second)
}

// stubDelayedDNSTT is a DNSTunnel that sets its own start delay.
type stubDelayedDNSTT struct {
	stubDNSTT
	delay time.Duration
}

func (s *stubDelayedDNSTT) StartDelay() time.Duration { return s.delay }

func TestDNSTunnelStartDelay(t *testing.T) {
	t.Parallel()
	delayOf := func(t *testing.T, opts ...Option) time.Duration {
		t.Helper()
		ki, err := NewKindling("test", opts...)
		require.NoError(t, err)
		k := ki.(*kindling)
		rt := k.newRaceTransport(k.transports)
		require.Len(t, k.transports, 1)
		return rt.startDelayOf(k.transports[0])
	}

	t.Run("Default", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, DefaultDNSTunnelStartDelay, delayOf(t, WithDNSTunnel(&stubDNSTT{})))
	})
	t.Run("TunnelSetsItsOwn", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 3*time.Second, delayOf(t, WithDNSTunnel(&stubDelayedDNSTT{delay: 3 * time.Second})))
	})
	t.Run("WithStartDelayOverrides", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 5*time.Second, delayOf(t,
			WithStartDelay(TransportDNSTunnel, 5*time.Second),
			WithDNSTunnel(&stubDelayedDNSTT{delay: 3 * time.Second})))
	})
	t.Run("WithStartDelayZero", func(t *testing.T) {
		t.Parallel()
		assert.Zero(t, delayOf(t, WithDNSTunnel(&stubDNSTT{}), WithStartDelay(TransportDNSTunnel, 0)))
	})
	t.Run("NegativeRejected", func(t *testing.T) {
		t.Parallel()
		_, err := NewKindling("test", WithDNSTunnel(&stubDNSTT{}), WithStartDelay(TransportDNSTunnel, -time.Second))
		assert.Error(t, err)
	})
	t.Run("ReplaceTransportKeepsIt", func(t *testing.T) {
		t.Parallel()
		ki, err := NewKindling("test", WithDNSTunnel(&stubDNSTT{}))
		require.NoError(t, err)
		require.NoError(t, ki.ReplaceTransport(TransportDNSTunnel, returns(http.StatusOK)))
		assert.Equal(t, DefaultDNSTunnelStartDelay, startDelayOf(ki.(*kindling).transports[0]))
	})
}
