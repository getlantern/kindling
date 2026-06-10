package kindling

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testLog = slog.New(slog.NewTextHandler(os.Stderr, nil))

// mockTransport is a test implementation of Transport that records whether it was called.
type mockTransport struct {
	name            string
	isStreamable    bool
	maxLength       int
	reqTimeout      time.Duration
	priority        int
	newRoundTripper func(ctx context.Context, addr string) (http.RoundTripper, error)
}

func (m *mockTransport) Name() string                  { return m.name }
func (m *mockTransport) IsStreamable() bool            { return m.isStreamable }
func (m *mockTransport) MaxLength() int                { return m.maxLength }
func (m *mockTransport) RequestTimeout() time.Duration { return m.reqTimeout }
func (m *mockTransport) Priority() int                 { return m.priority }
func (m *mockTransport) NewRoundTripper(ctx context.Context, addr string) (http.RoundTripper, error) {
	return m.newRoundTripper(ctx, addr)
}

func TestRaceTransport_StreamingHeaderFilter(t *testing.T) {
	t.Parallel()

	t.Run("StreamingRequest_SkipsNonStreamableTransport", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		var streamableCalled, nonStreamableCalled atomic.Bool
		rt := newRaceTransport("test", testLog, func(string) {},
			[]Transport{
				&mockTransport{
					name:         "streamable",
					isStreamable: true,
					newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
						streamableCalled.Store(true)
						return server.Client().Transport, nil
					},
				},
				&mockTransport{
					name:         "non-streamable",
					isStreamable: false,
					newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
						nonStreamableCalled.Store(true)
						return server.Client().Transport, nil
					},
				},
			},
		)

		req, err := http.NewRequest("GET", server.URL, nil)
		require.NoError(t, err)
		req.Header.Set("Accept", "text/event-stream")

		resp, err := rt.RoundTrip(req)
		require.NoError(t, err)
		resp.Body.Close()

		assert.True(t, streamableCalled.Load(), "streamable transport should be used for streaming request")
		assert.False(t, nonStreamableCalled.Load(), "non-streamable transport should be skipped for streaming request")
	})

	t.Run("StreamingRequest_UsesStreamableTransport", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		var called atomic.Bool
		rt := newRaceTransport("test", testLog, func(string) {},
			[]Transport{
				&mockTransport{
					name:         "streamable",
					isStreamable: true,
					newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
						called.Store(true)
						return server.Client().Transport, nil
					},
				},
			},
		)

		req, err := http.NewRequest("GET", server.URL, nil)
		require.NoError(t, err)
		req.Header.Set("Accept", "text/event-stream")

		resp, err := rt.RoundTrip(req)
		require.NoError(t, err)
		resp.Body.Close()

		assert.True(t, called.Load(), "streamable transport should be called for streaming request")
	})

	t.Run("NonStreamingRequest_AllowsNonStreamableTransport", func(t *testing.T) {
		t.Parallel()

		var streamableCalled, nonStreamableCalled atomic.Bool
		rt := newRaceTransport("test", testLog, func(string) {},
			[]Transport{
				&mockTransport{
					name:         "streamable",
					isStreamable: true,
					newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
						streamableCalled.Store(true)
						return nil, errors.New("intentional error")
					},
				},
				&mockTransport{
					name:         "non-streamable",
					isStreamable: false,
					newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
						nonStreamableCalled.Store(true)
						return nil, errors.New("intentional error")
					},
				},
			},
		)

		req, err := http.NewRequest("GET", "http://example.com", nil)
		require.NoError(t, err)

		_, err = rt.RoundTrip(req)
		require.Error(t, err, "expected error when all transports fail")

		assert.True(t, streamableCalled.Load(), "streamable transport should be attempted for non-streaming request")
		assert.True(t, nonStreamableCalled.Load(), "non-streamable transport should be attempted for non-streaming request")
	})
}

func TestRaceTransport_NoEligibleTransports(t *testing.T) {
	t.Parallel()

	t.Run("AllFilteredBySize", func(t *testing.T) {
		t.Parallel()

		rt := newRaceTransport("test", testLog, func(string) {},
			[]Transport{
				&mockTransport{
					name:      "limited",
					maxLength: 10,
					newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
						t.Error("should not be called")
						return nil, nil
					},
				},
			},
		)

		largeBody := strings.Repeat("x", 100)
		req, err := http.NewRequest("POST", "http://example.com", strings.NewReader(largeBody))
		require.NoError(t, err)

		_, err = rt.RoundTrip(req)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no eligible transports")
	})

	t.Run("EmptyTransportList", func(t *testing.T) {
		t.Parallel()

		rt := newRaceTransport("test", testLog, func(string) {}, nil)

		req, err := http.NewRequest("GET", "http://example.com", nil)
		require.NoError(t, err)

		_, err = rt.RoundTrip(req)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no eligible transports")
	})
}

func TestRaceTransport_AllTransportsFail(t *testing.T) {
	t.Parallel()

	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			&mockTransport{
				name: "fail-1",
				newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
					return nil, errors.New("connect error 1")
				},
			},
			&mockTransport{
				name: "fail-2",
				newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
					return nil, errors.New("connect error 2")
				},
			},
		},
	)

	req, err := http.NewRequest("GET", "http://example.com", nil)
	require.NoError(t, err)

	_, err = rt.RoundTrip(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all transports failed")
}

func TestRaceTransport_FirstSuccessWins(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	var slowCalled atomic.Bool
	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			&mockTransport{
				name: "fast",
				newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
					return server.Client().Transport, nil
				},
			},
			&mockTransport{
				name: "slow",
				newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
					<-ctx.Done()
					slowCalled.Store(true)
					return nil, ctx.Err()
				},
			},
		},
	)

	req, err := http.NewRequest("GET", server.URL, nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestRaceTransport_PanicRecovery(t *testing.T) {
	t.Parallel()

	var panicMsg string
	rt := newRaceTransport("test", testLog, func(msg string) { panicMsg = msg },
		[]Transport{
			&mockTransport{
				name: "panicker",
				newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
					panic("boom")
				},
			},
		},
	)

	req, err := http.NewRequest("GET", "http://example.com", nil)
	require.NoError(t, err)

	_, err = rt.RoundTrip(req)
	require.Error(t, err)
	assert.Contains(t, panicMsg, "boom")
}

// 4xx is the server's verdict on the request — replaying the request body
// on another transport would mean a non-idempotent handler ran twice. Pin
// the contract: 4xx returns to the caller, no second transport gets hit.
func TestRaceTransport_FourXX_NotRetried(t *testing.T) {
	t.Parallel()

	var firstHits, secondHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		http.Error(w, "verify failed", http.StatusUnprocessableEntity)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	// Delay the second transport's connection so the first wins the race
	// deterministically. `connected` lets us prove the second transport's
	// connect goroutine completed before we assert RoundTrip wasn't called.
	delayed, connected := delayedTransport("second", second.URL, 50*time.Millisecond)
	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			redirectTransport("first", first.URL),
			delayed,
		},
	)

	req, err := http.NewRequest("POST", "http://example.com/peer/verify", strings.NewReader(`{}`))
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode,
		"4xx response from the first transport must be returned to the caller")
	assert.Equal(t, int64(1), firstHits.Load(), "first transport must see exactly one request")
	waitForConnected(t, connected)
	assert.Equal(t, int64(0), secondHits.Load(),
		"second transport must NOT receive the request — replaying a non-idempotent body is the bug we're guarding against")
}

// 5xx is also the server's verdict (or an upstream proxy's) — same
// reasoning as 4xx. Document the contract for both.
func TestRaceTransport_FiveXX_NotRetried(t *testing.T) {
	t.Parallel()

	var firstHits, secondHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	delayed, connected := delayedTransport("second", second.URL, 50*time.Millisecond)
	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			redirectTransport("first", first.URL),
			delayed,
		},
	)

	req, err := http.NewRequest("POST", "http://example.com/peer/verify", strings.NewReader(`{}`))
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, int64(1), firstHits.Load())
	waitForConnected(t, connected)
	assert.Equal(t, int64(0), secondHits.Load())
}

// Idempotent-method exception: 5xx on a GET is allowed to fall back to
// the next transport. Real-world driver: a domain-front returning 5xx
// because it's being blocked, while the origin would happily respond.
// GET is safe to replay; the server has no side effects to replay.
func TestRaceTransport_FiveXX_RetriedForGET(t *testing.T) {
	t.Parallel()

	var firstHits, secondHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		http.Error(w, "blocked", http.StatusBadGateway)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer second.Close()

	// No explicit waitForConnected here — `secondHits == 1` is itself
	// implicit synchronization: raceTransport must wait for the delayed
	// transport's connectResult before issuing the second RoundTrip.
	delayed, _ := delayedTransport("second", second.URL, 50*time.Millisecond)
	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			redirectTransport("first", first.URL),
			delayed,
		},
	)

	req, err := http.NewRequest(http.MethodGet, "http://example.com/config-new", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"GET must fall back from 502 (first) to 200 (second)")
	assert.Equal(t, int64(1), firstHits.Load())
	assert.Equal(t, int64(1), secondHits.Load())
}

// Idempotent-method exception extended to HEAD.
func TestRaceTransport_TransportError_RetriedForHEAD(t *testing.T) {
	t.Parallel()

	var secondHits atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	delayed, _ := delayedTransport("second", second.URL, 50*time.Millisecond)
	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			&mockTransport{
				name: "rt-error",
				newRoundTripper: func(_ context.Context, _ string) (http.RoundTripper, error) {
					return errorRoundTripper{err: errors.New("write: connection reset")}, nil
				},
			},
			delayed,
		},
	)

	req, err := http.NewRequest(http.MethodHead, "http://example.com/health", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(1), secondHits.Load(),
		"HEAD must fall back through a transport-level RoundTrip error")
}

// 4xx is the server's verdict on the request itself — retrying won't make
// "your auth is wrong" or "no such resource" any more right. Even for GET,
// short-circuit on 4xx.
func TestRaceTransport_FourXX_NotRetried_EvenForGET(t *testing.T) {
	t.Parallel()

	var firstHits, secondHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	delayed, connected := delayedTransport("second", second.URL, 50*time.Millisecond)
	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			redirectTransport("first", first.URL),
			delayed,
		},
	)

	req, err := http.NewRequest(http.MethodGet, "http://example.com/missing", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, int64(1), firstHits.Load())
	waitForConnected(t, connected)
	assert.Equal(t, int64(0), secondHits.Load(),
		"4xx on GET must not retry — the request, not the transport, is the problem")
}

func TestIsRetryableMethod(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method string
		want   bool
	}{
		{http.MethodGet, true},
		{http.MethodHead, true},
		{"", true}, // net/http defaults to GET
		{http.MethodPost, false},
		{http.MethodPut, false},
		{http.MethodDelete, false},
		{http.MethodPatch, false},
		{http.MethodOptions, false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, isRetryableMethod(c.method), "method=%q", c.method)
	}
}

func TestGroupByPriority(t *testing.T) {
	t.Parallel()

	t.Run("AllDefault_SingleTier", func(t *testing.T) {
		t.Parallel()
		in := []Transport{&mockTransport{name: "a"}, &mockTransport{name: "b"}}
		tiers := groupByPriority(in)
		require.Len(t, tiers, 1)
		assert.Equal(t, []string{"a", "b"}, names(tiers[0]),
			"a single default tier must preserve input order")
	})

	t.Run("OrdersTiersAscending_PreservesIntraTierOrder", func(t *testing.T) {
		t.Parallel()
		in := []Transport{
			&mockTransport{name: "dnstt", priority: priorityLastResort},
			&mockTransport{name: "domainfront"},
			&mockTransport{name: "amp"},
			&mockTransport{name: "mid", priority: 50},
		}
		tiers := groupByPriority(in)
		require.Len(t, tiers, 3)
		assert.Equal(t, []string{"domainfront", "amp"}, names(tiers[0]), "default tier first, input order preserved")
		assert.Equal(t, []string{"mid"}, names(tiers[1]), "priority 50 in the middle")
		assert.Equal(t, []string{"dnstt"}, names(tiers[2]), "last-resort tier last")
	})

	t.Run("Empty", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, groupByPriority(nil))
	})
}

func names(transports []Transport) []string {
	out := make([]string, len(transports))
	for i, tr := range transports {
		out[i] = tr.Name()
	}
	return out
}

type errorRoundTripper struct{ err error }

func (e errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, e.err
}

// IdempotentHeader is the opt-in escape hatch for non-idempotent-by-method
// requests that the caller knows are safe to replay (e.g. /config-new POST,
// which is a read-only fetch dressed up as a POST). When set, the request
// gets the same retry-across-transports behavior as a GET.
func TestRaceTransport_IdempotentHeader_RetriesPOSTOn5xx(t *testing.T) {
	t.Parallel()

	var firstHits, secondHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		http.Error(w, "blocked", http.StatusBadGateway)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer second.Close()

	delayed, _ := delayedTransport("second", second.URL, 50*time.Millisecond)
	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			redirectTransport("first", first.URL),
			delayed,
		},
	)

	req, err := http.NewRequest(http.MethodPost, "http://example.com/config-new", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set(IdempotentHeader, "1")

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"POST tagged idempotent must fall back from 502 (first) to 200 (second)")
	assert.Equal(t, int64(1), firstHits.Load())
	assert.Equal(t, int64(1), secondHits.Load())
}

// Without the opt-in header, POST is single-shot — same as before. Pin
// this so we don't accidentally widen the override to all POSTs.
func TestRaceTransport_NoIdempotentHeader_POSTStillSingleShot(t *testing.T) {
	t.Parallel()

	var firstHits, secondHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	delayed, connected := delayedTransport("second", second.URL, 50*time.Millisecond)
	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			redirectTransport("first", first.URL),
			delayed,
		},
	)

	req, err := http.NewRequest(http.MethodPost, "http://example.com/peer/verify", strings.NewReader(`{}`))
	require.NoError(t, err)
	// Note: IdempotentHeader is intentionally NOT set.

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, int64(1), firstHits.Load())
	waitForConnected(t, connected)
	assert.Equal(t, int64(0), secondHits.Load(),
		"POST without IdempotentHeader must remain single-shot")
}

// Empty-string header value should NOT enable the override. Treat the
// header strictly as "non-empty value = idempotent" so accidentally
// setting an empty string doesn't silently re-enable replay.
func TestRaceTransport_EmptyIdempotentHeader_POSTStillSingleShot(t *testing.T) {
	t.Parallel()

	var firstHits, secondHits atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstHits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	delayed, connected := delayedTransport("second", second.URL, 50*time.Millisecond)
	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			redirectTransport("first", first.URL),
			delayed,
		},
	)

	req, err := http.NewRequest(http.MethodPost, "http://example.com/peer/verify", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set(IdempotentHeader, "")

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, int64(1), firstHits.Load())
	waitForConnected(t, connected)
	assert.Equal(t, int64(0), secondHits.Load())
}

// Connection-establishment failures must still fall back to the next
// transport — that's the whole point of racing transports. Only retries
// after a successful connect are forbidden for non-idempotent methods.
func TestRaceTransport_ConnectFailure_DoesFallBack(t *testing.T) {
	t.Parallel()

	var secondHits atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			&mockTransport{
				name: "fail-to-connect",
				newRoundTripper: func(_ context.Context, _ string) (http.RoundTripper, error) {
					return nil, errors.New("connect refused")
				},
			},
			redirectTransport("second", second.URL),
		},
	)

	req, err := http.NewRequest("POST", "http://example.com/anything", strings.NewReader(`{}`))
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(1), secondHits.Load(),
		"connect failure on the first transport must fall back to the second — no body has been transmitted")
}

// A last-resort transport (higher Priority) must not even be dialed while a
// default-tier transport can serve the request. This is the whole point of
// reserving slow fallbacks like DNS tunneling: they shouldn't compete on equal
// footing with the fast transports.
func TestRaceTransport_LastResort_NotDialedWhenDefaultSucceeds(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var lastResortDialed atomic.Bool
	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			&mockTransport{
				name: "default",
				newRoundTripper: func(_ context.Context, _ string) (http.RoundTripper, error) {
					return server.Client().Transport, nil
				},
			},
			&mockTransport{
				name:     "last-resort",
				priority: priorityLastResort,
				newRoundTripper: func(_ context.Context, _ string) (http.RoundTripper, error) {
					lastResortDialed.Store(true)
					return server.Client().Transport, nil
				},
			},
		},
	)

	req, err := http.NewRequest("GET", server.URL, nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, lastResortDialed.Load(),
		"last-resort transport must not be dialed while the default tier succeeds")
}

// When every default-tier transport fails to produce a usable response, the
// race must fall back to the last-resort tier — and only then dial it.
func TestRaceTransport_LastResort_UsedWhenDefaultFails(t *testing.T) {
	t.Parallel()

	var lastResortHits atomic.Int64
	lastResortSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lastResortHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer lastResortSrv.Close()

	// dialSeq records the order in which transports are dialed so we can assert
	// the last-resort tier is only reached after the default tier.
	var dialSeq atomic.Int64
	var defaultDialOrder, lastResortDialOrder atomic.Int64

	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			&mockTransport{
				name: "default",
				newRoundTripper: func(_ context.Context, _ string) (http.RoundTripper, error) {
					defaultDialOrder.Store(dialSeq.Add(1))
					return nil, errors.New("connect refused")
				},
			},
			&mockTransport{
				name:     "last-resort",
				priority: priorityLastResort,
				newRoundTripper: func(_ context.Context, _ string) (http.RoundTripper, error) {
					lastResortDialOrder.Store(dialSeq.Add(1))
					return &urlRewritingTransport{target: lastResortSrv.URL}, nil
				},
			},
		},
	)

	req, err := http.NewRequest("GET", "http://example.com/anything", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(1), lastResortHits.Load(), "last-resort transport should have served the request")
	assert.Equal(t, int64(1), defaultDialOrder.Load(), "default tier should be dialed first")
	assert.Equal(t, int64(2), lastResortDialOrder.Load(), "last-resort tier should be dialed only after the default tier")
}

// A 5xx from the default tier is a retryable failure, so the race must still
// fall through to the last-resort tier rather than returning the 5xx while a
// working fallback exists.
func TestRaceTransport_LastResort_UsedWhenDefaultReturns5xx(t *testing.T) {
	t.Parallel()

	defaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer defaultSrv.Close()

	var lastResortHits atomic.Int64
	lastResortSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lastResortHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer lastResortSrv.Close()

	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			redirectTransport("default", defaultSrv.URL),
			&mockTransport{
				name:     "last-resort",
				priority: priorityLastResort,
				newRoundTripper: func(_ context.Context, _ string) (http.RoundTripper, error) {
					return &urlRewritingTransport{target: lastResortSrv.URL}, nil
				},
			},
		},
	)

	req, err := http.NewRequest("GET", "http://example.com/anything", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(1), lastResortHits.Load(),
		"a retryable 5xx in the default tier should fall through to the last-resort tier")
}

// A usable (held) 5xx from an earlier tier must survive a later tier timing
// out: the request budget is shared, so when the last-resort tier can't beat
// the deadline we should still return the earlier tier's response rather than
// a bare context-deadline error.
func TestRaceTransport_LastResort_TimeoutKeepsEarlierTier5xx(t *testing.T) {
	t.Parallel()

	defaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer defaultSrv.Close()

	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			redirectTransport("default", defaultSrv.URL),
			&mockTransport{
				name:     "last-resort",
				priority: priorityLastResort,
				newRoundTripper: func(ctx context.Context, _ string) (http.RoundTripper, error) {
					// Never connects within the deadline.
					<-ctx.Done()
					return nil, ctx.Err()
				},
			},
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://example.com/anything", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err, "must surface the earlier tier's 5xx, not the timeout error")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// A non-idempotent request whose entire default tier fails to *connect* may
// safely fall through to the last-resort tier: no body has crossed the wire,
// so there is no replay risk. This is the one path that replays a POST body on
// a later tier.
func TestRaceTransport_LastResort_NonIdempotentFallsThroughOnConnectFailure(t *testing.T) {
	t.Parallel()

	var lastResortHits atomic.Int64
	lastResortSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lastResortHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer lastResortSrv.Close()

	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			&mockTransport{
				name: "default",
				newRoundTripper: func(_ context.Context, _ string) (http.RoundTripper, error) {
					return nil, errors.New("connect refused")
				},
			},
			&mockTransport{
				name:     "last-resort",
				priority: priorityLastResort,
				newRoundTripper: func(_ context.Context, _ string) (http.RoundTripper, error) {
					return &urlRewritingTransport{target: lastResortSrv.URL}, nil
				},
			},
		},
	)

	req, err := http.NewRequest("POST", "http://example.com/anything", strings.NewReader(`{}`))
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(1), lastResortHits.Load(),
		"a connection failure carries no replay risk, so a non-idempotent request may fall through to the last-resort tier")
}

// A 5xx on a non-idempotent request is single-shot: it must be returned as-is,
// NOT retried on the last-resort tier, because the body already crossed the
// wire and the server may have applied a side effect. Contrast with the
// idempotent case in TestRaceTransport_LastResort_UsedWhenDefaultReturns5xx.
func TestRaceTransport_LastResort_NonIdempotent5xxIsSingleShot(t *testing.T) {
	t.Parallel()

	defaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer defaultSrv.Close()

	var lastResortDialed atomic.Bool
	rt := newRaceTransport("test", testLog, func(string) {},
		[]Transport{
			redirectTransport("default", defaultSrv.URL),
			&mockTransport{
				name:     "last-resort",
				priority: priorityLastResort,
				newRoundTripper: func(_ context.Context, _ string) (http.RoundTripper, error) {
					lastResortDialed.Store(true)
					return &urlRewritingTransport{target: defaultSrv.URL}, nil
				},
			},
		},
	)

	req, err := http.NewRequest("POST", "http://example.com/anything", strings.NewReader(`{}`))
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode, "non-idempotent 5xx is returned as-is")
	assert.False(t, lastResortDialed.Load(),
		"a non-idempotent 5xx must not fall through to the last-resort tier — replay risk")
}

// redirectTransport returns a Transport whose RoundTripper rewrites the
// request URL to point at testServerURL, so we can stand up real httptest
// servers per transport and observe how many times each is hit.
func redirectTransport(name, testServerURL string) Transport {
	return &mockTransport{
		name: name,
		newRoundTripper: func(_ context.Context, _ string) (http.RoundTripper, error) {
			return &urlRewritingTransport{target: testServerURL}, nil
		},
	}
}

// delayedTransport returns a Transport whose NewRoundTripper sleeps for
// `delay` before returning, plus a `connected` channel that is closed once
// NewRoundTripper has returned (regardless of outcome). Tests that assert
// the delayed transport's request handler was NOT hit can wait on
// `connected` to deterministically know the connect goroutine has finished
// — at which point raceTransport has already observed the connectResult,
// and either a RoundTrip has fired or it never will.
func delayedTransport(name, testServerURL string, delay time.Duration) (Transport, <-chan struct{}) {
	connected := make(chan struct{})
	return &mockTransport{
		name: name,
		newRoundTripper: func(ctx context.Context, _ string) (http.RoundTripper, error) {
			defer close(connected)
			select {
			case <-time.After(delay):
				return &urlRewritingTransport{target: testServerURL}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}, connected
}

// waitForConnected blocks until ch is closed or fails the test if the
// configured grace period elapses first. The grace period is deliberately
// generous; a hung delayed transport is a test bug, not a race we want to
// paper over.
func waitForConnected(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("delayed transport never finished its connect attempt")
	}
}

type urlRewritingTransport struct{ target string }

func (u *urlRewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target := u.target + req.URL.Path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	parsed, err := http.NewRequestWithContext(req.Context(), req.Method, target, req.Body)
	if err != nil {
		return nil, err
	}
	parsed.Header = req.Header.Clone()
	return http.DefaultTransport.RoundTrip(parsed)
}

func TestCloneRequest_NilBody(t *testing.T) {
	req, err := http.NewRequest("GET", "http://example.com", nil)
	require.NoError(t, err)

	cloned := cloneRequest(req, "test", "test", nil)
	assert.NotSame(t, req, cloned)
	assert.True(t, cloned.Body == nil || cloned.Body == http.NoBody,
		"expected nil or NoBody, got %v", cloned.Body)
}

func TestCloneRequest_NoBody(t *testing.T) {
	req, err := http.NewRequest("GET", "http://example.com", http.NoBody)
	require.NoError(t, err)

	cloned := cloneRequest(req, "test", "test", nil)
	assert.NotSame(t, req, cloned)
	assert.Equal(t, http.NoBody, cloned.Body)
}

func TestCloneRequest_WithBody(t *testing.T) {
	originalBody := "hello world"
	req, err := http.NewRequest("POST", "http://example.com", io.NopCloser(strings.NewReader(originalBody)))
	require.NoError(t, err)

	bodyBytes, err := io.ReadAll(req.Body)
	require.NoError(t, err)

	cloned := cloneRequest(req, "test", "method-x", bodyBytes)

	// Verify cloned body matches original content.
	clonedBody, err := io.ReadAll(cloned.Body)
	require.NoError(t, err)
	assert.Equal(t, originalBody, string(clonedBody))

	// Verify Kindling headers are set.
	assert.Equal(t, "test", cloned.Header.Get("X-Kindling-App"))
	assert.Equal(t, "method-x", cloned.Header.Get("X-Kindling-Method"))

	// Verify ContentLength is set correctly.
	assert.Equal(t, int64(len(originalBody)), cloned.ContentLength)
}

func TestHostWithPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host, scheme, want string
	}{
		{"example.com", "https", "example.com:443"},
		{"example.com", "http", "example.com:80"},
		{"example.com:8080", "https", "example.com:8080"},
		{"example.com:8080", "http", "example.com:8080"},
		{"[::1]", "https", "[::1]:443"},
		{"[::1]", "http", "[::1]:80"},
		{"[::1]:8080", "https", "[::1]:8080"},
	}
	for _, tt := range tests {
		got := hostWithPort(tt.host, tt.scheme)
		assert.Equal(t, tt.want, got, "hostWithPort(%q, %q)", tt.host, tt.scheme)
	}
}

func TestDrainRequestBody(t *testing.T) {
	t.Parallel()

	t.Run("NilBody", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://example.com", nil)
		require.NoError(t, err)
		body, err := drainRequestBody(req)
		require.NoError(t, err)
		assert.Nil(t, body)
	})

	t.Run("WithContent", func(t *testing.T) {
		content := "request body"
		req, err := http.NewRequest("POST", "http://example.com", strings.NewReader(content))
		require.NoError(t, err)
		body, err := drainRequestBody(req)
		require.NoError(t, err)
		assert.Equal(t, content, string(body))

		// Verify body was restored on the request.
		restored, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		assert.Equal(t, content, string(restored))
	})
}

func TestRequestTimeout(t *testing.T) {
	t.Parallel()

	t.Run("NoContent", func(t *testing.T) {
		rt := &raceTransport{}
		req, err := http.NewRequest("GET", "http://example.com", nil)
		require.NoError(t, err)
		assert.Equal(t, 80*time.Second, rt.requestTimeout(req, rt.transports))
	})

	t.Run("WithContent", func(t *testing.T) {
		rt := &raceTransport{}
		req, err := http.NewRequest("POST", "http://example.com",
			bytes.NewReader(make([]byte, 1000)))
		require.NoError(t, err)
		req.ContentLength = 1000
		assert.Equal(t, 3*time.Minute, rt.requestTimeout(req, rt.transports))
	})

	t.Run("TransportOverridesBase", func(t *testing.T) {
		rt := &raceTransport{
			transports: []Transport{
				&mockTransport{name: "slow", reqTimeout: 5 * time.Minute},
			},
		}
		req, _ := http.NewRequest("GET", "http://example.com", nil)
		assert.Equal(t, 5*time.Minute, rt.requestTimeout(req, rt.transports))
	})

	t.Run("TakesMaxAcrossTransports", func(t *testing.T) {
		rt := &raceTransport{
			transports: []Transport{
				&mockTransport{name: "fast", reqTimeout: 2 * time.Minute},
				&mockTransport{name: "slow", reqTimeout: 5 * time.Minute},
				&mockTransport{name: "medium", reqTimeout: 3 * time.Minute},
			},
		}
		req, _ := http.NewRequest("POST", "http://example.com",
			bytes.NewReader(make([]byte, 1000)))
		req.ContentLength = 1000
		assert.Equal(t, 5*time.Minute, rt.requestTimeout(req, rt.transports))
	})

	t.Run("IgnoresFilteredTransports", func(t *testing.T) {
		// The slow transport caps its body size below this request, so
		// filterTransports drops it and its long timeout must not leak
		// into the budget.
		rt := &raceTransport{
			log: slog.Default(),
			transports: []Transport{
				&mockTransport{name: "fast", reqTimeout: 4 * time.Minute},
				&mockTransport{name: "slow", reqTimeout: 10 * time.Minute, maxLength: 10},
			},
		}
		req, _ := http.NewRequest("POST", "http://example.com",
			bytes.NewReader(make([]byte, 1000)))
		req.ContentLength = 1000
		eligible := rt.filterTransports(req, make([]byte, 1000))
		// "slow" is filtered out, so its 10m timeout must not apply; the
		// budget comes from the eligible "fast" transport instead.
		assert.Equal(t, 4*time.Minute, rt.requestTimeout(req, eligible))
	})
}
