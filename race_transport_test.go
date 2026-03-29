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
	newRoundTripper func(ctx context.Context, addr string) (http.RoundTripper, error)
}

func (m *mockTransport) Name() string       { return m.name }
func (m *mockTransport) IsStreamable() bool { return m.isStreamable }
func (m *mockTransport) MaxLength() int     { return m.maxLength }
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
		req, err := http.NewRequest("GET", "http://example.com", nil)
		require.NoError(t, err)
		assert.Equal(t, 80*time.Second, requestTimeout(req))
	})

	t.Run("WithContent", func(t *testing.T) {
		req, err := http.NewRequest("POST", "http://example.com",
			bytes.NewReader(make([]byte, 1000)))
		require.NoError(t, err)
		req.ContentLength = 1000
		assert.Equal(t, 3*time.Minute, requestTimeout(req))
	})
}
