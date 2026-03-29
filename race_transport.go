package kindling

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// raceTransport is an http.RoundTripper that races requests across multiple
// transports. Connections are established concurrently, but requests are sent
// serially in the order transports connect — avoiding issues with
// non-idempotent requests.
type raceTransport struct {
	transports    []Transport
	panicListener func(string)
	appName       string
	log           *slog.Logger
}

func newRaceTransport(appName string, log *slog.Logger, panicListener func(string), transports []Transport) *raceTransport {
	return &raceTransport{
		transports:    transports,
		panicListener: panicListener,
		appName:       appName,
		log:           log,
	}
}

// connectResult holds the outcome of a single transport connection attempt.
type connectResult struct {
	rt   http.RoundTripper
	name string
	err  error
}

func (t *raceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), requestTimeout(req))
	defer cancel()

	bodyBytes, err := drainRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("reading request body: %w", err)
	}

	eligible := t.filterTransports(req, bodyBytes)
	if len(eligible) == 0 {
		return nil, errors.New("no eligible transports for request")
	}

	t.log.Debug("Racing transports",
		"count", len(eligible),
		"bodyLength", len(bodyBytes),
	)

	// Connect all eligible transports in parallel. Each goroutine sends
	// exactly one result, so the channel will receive len(eligible) messages.
	results := make(chan connectResult, len(eligible))
	addr := hostWithPort(req.URL.Host, req.URL.Scheme)
	for _, tr := range eligible {
		go t.connect(ctx, tr, addr, results)
	}

	// Try connected transports as they arrive. Because we consume results
	// one at a time, requests are sent serially.
	var lastResp *http.Response
	var lastErr error

	for remaining := len(eligible); remaining > 0; remaining-- {
		select {
		case result := <-results:
			if result.err != nil {
				t.log.Error("Transport connection failed",
					"name", result.name,
					"error", result.err,
				)
				lastErr = result.err
				continue
			}

			t.log.Debug("Transport connected", "name", result.name)
			clone := cloneRequest(req, t.appName, result.name, bodyBytes)
			resp, err := result.rt.RoundTrip(clone)
			if err != nil {
				t.log.Error("HTTP request failed",
					"name", result.name,
					"error", err,
				)
				lastErr = err
				continue
			}

			if resp.StatusCode < http.StatusBadRequest {
				t.log.Debug("Request succeeded",
					"name", result.name,
					"status", resp.StatusCode,
				)
				return resp, nil
			}

			// Retryable status — close previous failed response, keep this one
			// in case no transport succeeds.
			t.log.Warn("Retryable HTTP status",
				"name", result.name,
				"status", resp.StatusCode,
			)
			if lastResp != nil {
				io.Copy(io.Discard, lastResp.Body)
				lastResp.Body.Close()
			}
			lastResp = resp
			lastErr = fmt.Errorf("transport %s: http status %d", result.name, resp.StatusCode)

		case <-ctx.Done():
			if lastResp != nil {
				return lastResp, nil
			}
			if lastErr != nil {
				return nil, fmt.Errorf("timed out, last error: %w", lastErr)
			}
			return nil, ctx.Err()
		}
	}

	// All transports exhausted.
	if lastResp != nil {
		return lastResp, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("all transports failed: %w", lastErr)
	}
	return nil, errors.New("no transports produced a response")
}

// connect establishes a connection using the given transport and sends the
// result (success or failure) on the results channel. Panics are recovered.
func (t *raceTransport) connect(ctx context.Context, tr Transport, addr string, results chan<- connectResult) {
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("panic in transport %s: %v", tr.Name(), r)
			t.panicListener(msg)
			results <- connectResult{name: tr.Name(), err: errors.New(msg)}
		}
	}()

	rt, err := tr.NewRoundTripper(ctx, addr)
	if err != nil {
		results <- connectResult{name: tr.Name(), err: err}
		return
	}
	if ctx.Err() != nil {
		results <- connectResult{name: tr.Name(), err: ctx.Err()}
		return
	}
	results <- connectResult{rt: rt, name: tr.Name()}
}

// filterTransports returns only the transports eligible for this request,
// based on body size limits and streaming support.
func (t *raceTransport) filterTransports(req *http.Request, bodyBytes []byte) []Transport {
	isStreaming := req.Header.Get("Accept") == "text/event-stream"
	eligible := make([]Transport, 0, len(t.transports))
	for _, tr := range t.transports {
		if tr.MaxLength() > 0 && len(bodyBytes) > tr.MaxLength() {
			t.log.Debug("Skipping transport: body exceeds limit",
				"name", tr.Name(),
				"bodySize", len(bodyBytes),
				"maxLength", tr.MaxLength(),
			)
			continue
		}
		if isStreaming && !tr.IsStreamable() {
			t.log.Debug("Skipping non-streamable transport",
				"name", tr.Name(),
			)
			continue
		}
		eligible = append(eligible, tr)
	}
	return eligible
}

// hostWithPort ensures the host string includes a port, defaulting based on scheme.
// Handles bracketed IPv6 literals (e.g. "[::1]") that lack a port.
func hostWithPort(host, scheme string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	// Strip brackets from IPv6 literals so JoinHostPort doesn't double-bracket.
	if len(host) > 1 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	if scheme == "https" {
		return net.JoinHostPort(host, "443")
	}
	return net.JoinHostPort(host, "80")
}

// cloneRequest creates a copy of the HTTP request with the body replaced by
// bodyBytes and Kindling-specific tracing headers added.
func cloneRequest(req *http.Request, app, method string, bodyBytes []byte) *http.Request {
	clone := req.Clone(req.Context())
	clone.Header.Set("X-Kindling-App", app)
	clone.Header.Set("X-Kindling-Method", method)
	if req.Body != nil && req.Body != http.NoBody && len(bodyBytes) > 0 {
		clone.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		clone.ContentLength = int64(len(bodyBytes))
	}
	return clone
}

// requestTimeout returns the appropriate timeout for the request — longer
// for uploads with content, shorter for GETs and small requests.
func requestTimeout(req *http.Request) time.Duration {
	if req.Body != nil && req.Body != http.NoBody && req.ContentLength != 0 {
		return 3 * time.Minute
	}
	return 80 * time.Second
}

// drainRequestBody reads the full request body into a byte slice and restores
// it on the request. Returns nil for requests with no body.
func drainRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return bodyBytes, nil
}
