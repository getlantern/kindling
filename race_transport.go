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

// IdempotentHeader is an opt-in marker callers can set on a request to
// declare it idempotent regardless of HTTP method. raceTransport will
// then apply the same retry-across-transports behavior to that request
// that it normally reserves for GET/HEAD: transport-level errors and
// 5xx responses fall back to the next connected transport.
//
// Use this for POST endpoints that are semantically read-only or
// otherwise safe to replay (e.g., a config-fetch endpoint that returns
// the same payload regardless of how many times it's called). The
// header is harmless to leak to the origin — kindling doesn't strip
// it before sending.
//
// Any non-empty value enables the override. Recommended value is "1".
const IdempotentHeader = "X-Kindling-Idempotent"

// raceTransport is an http.RoundTripper that races *connections* across
// multiple transports. Connections are established concurrently; the first
// transport to connect receives the request, and the response is returned
// to the caller.
//
// Retry behavior is method-aware, with a per-request opt-in override:
//
//   - For idempotent methods (GET, HEAD), the original retry-across-transports
//     behavior is preserved: transport-level errors after RoundTrip and 5xx
//     responses fall back to the next connected transport. This handles the
//     case where an intermediary fronting transport (rather than the origin)
//     produced the 5xx — common when one fronting CDN is being blocked.
//     4xx responses are NOT retried even for idempotent methods: a 4xx is
//     the server's verdict on the request itself, so retrying won't help.
//
//   - For non-idempotent methods (POST/PUT/DELETE/PATCH/etc.), exactly one
//     request is sent once any transport connects, and the response is
//     returned regardless of status or transport error. Retrying would
//     risk replaying server-side side effects (DB writes, registrations,
//     billing events) — once the body has crossed the wire, the server
//     may have processed it even if the response was lost in transit.
//     PUT/DELETE are technically idempotent per RFC 7231 but are excluded
//     here because the side effect may have been applied before a transient
//     failure dropped the response, matching stdlib http.Client's stricter
//     view of which methods are safe to replay.
//
//   - Callers that know a non-idempotent-by-method request is in fact
//     safe to replay can opt back into the retry behavior by setting
//     [IdempotentHeader] on the request. This is intended for POSTs that
//     wrap idempotent reads (config fetches, status polls) where having
//     one fronting transport blocked shouldn't cause the call to fail.
//
// Connections that fail to establish always fall back to the next transport
// regardless of method — no body has been transmitted on a connection that
// never came up, so replay risk is zero.
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

	idempotent := isRetryableMethod(req.Method) || req.Header.Get(IdempotentHeader) != ""
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

			t.log.Debug("Transport connected, sending request", "name", result.name, "method", req.Method)
			clone := cloneRequest(req, t.appName, result.name, bodyBytes)
			resp, err := result.rt.RoundTrip(clone)

			if !idempotent {
				// Single-shot: return whatever happened. Retrying on a non-
				// idempotent method risks replaying side effects.
				return resp, err
			}

			if err != nil {
				t.log.Warn("HTTP request failed on idempotent method, falling back",
					"name", result.name,
					"method", req.Method,
					"error", err,
				)
				// http.RoundTripper allows resp to be non-nil even when
				// err is non-nil (the spec only requires resp == nil when
				// err == nil for the success direction). Drain + close
				// defensively so we don't leak the body / connection.
				if resp != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				lastErr = err
				continue
			}

			if resp.StatusCode >= 500 {
				// 5xx on an idempotent method — the response may be from a
				// blocked intermediary rather than the origin. Try the next
				// transport. Hold this response in case nothing else works.
				t.log.Warn("Retryable 5xx on idempotent method, falling back",
					"name", result.name,
					"method", req.Method,
					"status", resp.StatusCode,
				)
				if lastResp != nil {
					_, _ = io.Copy(io.Discard, lastResp.Body)
					_ = lastResp.Body.Close()
				}
				lastResp = resp
				lastErr = fmt.Errorf("transport %s: http status %d", result.name, resp.StatusCode)
				continue
			}

			// 2xx, 3xx, or 4xx on an idempotent method: 4xx is the server's
			// verdict on the request itself, retry won't help. Return.
			if lastResp != nil {
				_, _ = io.Copy(io.Discard, lastResp.Body)
				_ = lastResp.Body.Close()
			}
			return resp, nil

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

// isRetryableMethod reports whether requests with this method are safe to
// replay on a different transport after a transport-level error or 5xx
// response. Only GET and HEAD are included: they have no side effects
// (RFC 7231 §4.2.1 "safe" methods) and the stdlib http.Client uses the
// same conservative position. PUT/DELETE are technically idempotent per
// the RFC but a server may have applied the side effect before a transient
// failure dropped the response, so replaying them is unsafe.
func isRetryableMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, "":
		// Empty method defaults to GET in net/http.
		return true
	}
	return false
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
