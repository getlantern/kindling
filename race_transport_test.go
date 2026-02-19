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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	// Verifies that when the Accept header is "text/event-stream", only streamable
	// transports are used and non-streamable ones are skipped entirely.
	t.Run("StreamingRequest_SkipsNonStreamableTransport", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		var streamableCalled, nonStreamableCalled atomic.Bool
		rt := newRaceTransport("test", func(string) {},
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

	// Verifies that a streamable transport successfully handles a streaming
	// request end-to-end and returns a successful response.
	t.Run("StreamingRequest_UsesStreamableTransport", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		var called atomic.Bool
		rt := newRaceTransport("test", func(string) {},
			&mockTransport{
				name:         "streamable",
				isStreamable: true,
				newRoundTripper: func(ctx context.Context, addr string) (http.RoundTripper, error) {
					called.Store(true)
					return server.Client().Transport, nil
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

	// Verifies that the streaming filter is not applied for ordinary (non-streaming)
	// requests, so both streamable and non-streamable transports are attempted.
	t.Run("NonStreamingRequest_AllowsNonStreamableTransport", func(t *testing.T) {
		t.Parallel()

		var streamableCalled, nonStreamableCalled atomic.Bool
		rt := newRaceTransport("test", func(string) {},
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
		)

		req, err := http.NewRequest("GET", "http://example.com", nil)
		require.NoError(t, err)
		// No Accept: text/event-stream header — streaming filter must not apply.

		_, err = rt.RoundTrip(req)
		require.Error(t, err, "expected error when all transports fail")

		assert.True(t, streamableCalled.Load(), "streamable transport should be attempted for non-streaming request")
		assert.True(t, nonStreamableCalled.Load(), "non-streamable transport should be attempted for non-streaming request")
	})
}

func TestCloneRequest_NilBody(t *testing.T) {
	req, err := http.NewRequest("GET", "http://example.com", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	cloned := cloneRequest(req, "test", "test", []byte{})
	if cloned == req {
		t.Error("expected a new request, got the same pointer")
	}
	if cloned.Body != nil && cloned.Body != http.NoBody {
		t.Error("expected cloned body to be nil or http.NoBody")
	}
}

func TestCloneRequest_NoBody(t *testing.T) {
	req, err := http.NewRequest("GET", "http://example.com", http.NoBody)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	cloned := cloneRequest(req, "test", "test", []byte{})
	if cloned == req {
		t.Error("expected a new request, got the same pointer")
	}
	if cloned.Body != http.NoBody {
		t.Error("expected cloned body to be http.NoBody")
	}
}

func TestCloneRequest_WithBody(t *testing.T) {
	originalBody := "hello world"
	req, err := http.NewRequest("POST", "http://example.com", io.NopCloser(strings.NewReader(originalBody)))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	bodyBytes, _ := io.ReadAll(req.Body)

	cloned := cloneRequest(req, "test", "test", bodyBytes)

	// Both bodies should be readable and equal to originalBody
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	origBodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read original body: %v", err)
	}
	if string(origBodyBytes) != originalBody {
		t.Errorf("expected original body %q, got %q", originalBody, string(origBodyBytes))
	}

	clonedBodyBytes, err := io.ReadAll(cloned.Body)
	if err != nil {
		t.Fatalf("failed to read cloned body: %v", err)
	}
	if string(clonedBodyBytes) != originalBody {
		t.Errorf("expected cloned body %q, got %q", originalBody, string(clonedBodyBytes))
	}
}
