package kindling

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// stallingRT connects at once but never answers, like a fronted request whose
// packets are dropped after the TLS handshake. It reports when its request's
// context ends.
type stallingRT struct {
	sent      atomic.Bool
	cancelled chan struct{}
}

func newStallingRT() *stallingRT { return &stallingRT{cancelled: make(chan struct{})} }

func (s *stallingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	s.sent.Store(true)
	<-req.Context().Done()
	close(s.cancelled)
	return nil, req.Context().Err()
}

func (s *stallingRT) connect(context.Context, string) (http.RoundTripper, error) { return s, nil }

// A transport can connect and then hang on the request itself. A replay-safe
// request must not wait behind it: the late transport sends alongside, wins,
// and the stalled request is cancelled.
func TestStartDelay_RescuesStalledRequest(t *testing.T) {
	t.Parallel()
	// The body follows the headers after a pause, so it's still in flight
	// when the race returns.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "via soar")
	}))
	defer server.Close()

	stalled := newStallingRT()
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "domainfront", newRoundTripper: stalled.connect},
		&mockTransport{name: "soar", priority: priorityLastResort, startDelay: 50 * time.Millisecond,
			newRoundTripper: func(context.Context, string) (http.RoundTripper, error) {
				return &urlRewritingTransport{target: server.URL}, nil
			}},
	})
	req, err := http.NewRequest(http.MethodGet, "https://origin.example/v1/config", nil)
	require.NoError(t, err)
	begin := time.Now()
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Less(t, time.Since(begin), 5*time.Second, "soar waited behind the stalled request")
	assert.True(t, stalled.sent.Load())

	// The winner's request context must outlive the race, or its body would
	// be cut off.
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "via soar", string(body))

	select {
	case <-stalled.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled request was left running after soar won")
	}
}

// A request that isn't replay-safe is still sent exactly once, even when the
// transport it went to stalls and a late transport connects.
func TestStartDelay_NonIdempotentStaysSingleShot(t *testing.T) {
	t.Parallel()
	stalled := newStallingRT()
	var soarSent atomic.Bool
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "domainfront", newRoundTripper: stalled.connect},
		&mockTransport{name: "soar", priority: priorityLastResort, startDelay: 20 * time.Millisecond,
			newRoundTripper: func(context.Context, string) (http.RoundTripper, error) {
				return roundTripFunc(func(*http.Request) (*http.Response, error) {
					soarSent.Store(true)
					return statusRT(http.StatusOK).RoundTrip(nil)
				}), nil
			}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://origin.example/v1/register", nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	assert.Error(t, err, "the POST's only attempt stalled until the caller gave up")
	assert.False(t, soarSent.Load(), "a POST was sent a second time")
}

// The late transport can be the one that hangs. A transport that connects after
// it must not wait behind it.
func TestStartDelay_EarlierTransportNotQueuedBehindStalledLateOne(t *testing.T) {
	t.Parallel()
	stalled := newStallingRT()
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "domainfront", newRoundTripper: after(150*time.Millisecond, http.StatusOK)},
		&mockTransport{name: "soar", priority: priorityLastResort, startDelay: 20 * time.Millisecond,
			newRoundTripper: stalled.connect},
	})
	resp, err, took := roundTrip(t, rt, http.MethodGet)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, stalled.sent.Load(), "soar connected first, so it should have sent")
	assert.Less(t, took, 5*time.Second, "domainfront waited behind the stalled soar request")
}

// A connection queued behind a hung request is sent as soon as a transport
// joins late, not only when the hung request ends.
func TestStartDelay_QueuedConnectionSentWhenHedgingStarts(t *testing.T) {
	t.Parallel()
	stalled := newStallingRT()
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "domainfront", newRoundTripper: stalled.connect},
		&mockTransport{name: "smart", newRoundTripper: after(20*time.Millisecond, http.StatusOK)},
		&mockTransport{name: "soar", priority: priorityLastResort, startDelay: 100 * time.Millisecond, newRoundTripper: hangs},
	})
	resp, err, took := roundTrip(t, rt, http.MethodGet)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.GreaterOrEqual(t, took, 100*time.Millisecond, "smart sent alongside domainfront before anything joined late")
	assert.Less(t, took, 5*time.Second, "smart stayed queued after soar joined")
}

// rwBody is a writable response body, like the upgraded connection net/http
// returns for 101 Switching Protocols.
type rwBody struct {
	io.Reader
	written bytes.Buffer
	closed  bool
}

func (b *rwBody) Write(p []byte) (int, error) { return b.written.Write(p) }
func (b *rwBody) Close() error                { b.closed = true; return nil }

// Wrapping the winner's body for cancellation must keep it writable, or a
// caller can't use an upgraded connection.
func TestRaceTransport_UpgradedBodyStaysWritable(t *testing.T) {
	t.Parallel()
	body := &rwBody{Reader: strings.NewReader("")}
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "smart", newRoundTripper: func(context.Context, string) (http.RoundTripper, error) {
			return roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: body}, nil
			}), nil
		}},
	})
	req, err := http.NewRequest(http.MethodGet, "https://origin.example/ws", nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	require.True(t, ok, "the upgraded body lost its Write method")
	_, err = rwc.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, rwc.Close())
	assert.Equal(t, "hello", body.written.String())
	assert.True(t, body.closed)
}

// blockingBody never reaches EOF: Read blocks until the body is closed or ctx
// ends, like an upgraded connection or a stalled stream.
type blockingBody struct {
	ctx    context.Context
	closed chan struct{}
	once   atomic.Bool
}

func newBlockingBody(ctx context.Context) *blockingBody {
	return &blockingBody{ctx: ctx, closed: make(chan struct{})}
}

func (b *blockingBody) Read([]byte) (int, error) {
	select {
	case <-b.closed:
		return 0, io.ErrClosedPipe
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func (b *blockingBody) Close() error {
	if b.once.CompareAndSwap(false, true) {
		close(b.closed)
	}
	return nil
}

// A held retryable response whose body stalls must not block the race when a
// usable response arrives: its request is cancelled before the body is
// drained.
func TestRaceTransport_StalledHeldBodyDoesNotBlock(t *testing.T) {
	t.Parallel()
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "smart", newRoundTripper: func(context.Context, string) (http.RoundTripper, error) {
			return roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: newBlockingBody(req.Context())}, nil
			}), nil
		}},
		&mockTransport{name: "domainfront", newRoundTripper: after(50*time.Millisecond, http.StatusOK)},
	})
	resp, err, took := roundTrip(t, rt, http.MethodGet)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Less(t, took, 5*time.Second, "draining the held 503 blocked the race")
}

// A losing response that arrives after the race ends is closed even if its
// body never reaches EOF, as with an upgraded connection.
func TestRaceTransport_LateLoserBodyClosed(t *testing.T) {
	t.Parallel()
	loser := newBlockingBody(context.Background())
	rt := newRaceTransport("test", testLog, func(string) {}, []Transport{
		&mockTransport{name: "domainfront", newRoundTripper: func(context.Context, string) (http.RoundTripper, error) {
			return roundTripFunc(func(*http.Request) (*http.Response, error) {
				time.Sleep(200 * time.Millisecond) // answers after soar has won
				return &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: loser}, nil
			}), nil
		}},
		&mockTransport{name: "soar", priority: priorityLastResort, startDelay: 20 * time.Millisecond, newRoundTripper: returns(http.StatusOK)},
	})
	resp, err, _ := roundTrip(t, rt, http.MethodGet)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	select {
	case <-loser.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the losing response's body was never closed")
	}
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
