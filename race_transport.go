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
	"sort"
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
//
// Transports are raced in priority tiers (see [transportPriority]). All
// transports in the lowest-numbered tier race in parallel as described above;
// a higher-numbered tier is started only once every transport in all
// lower-numbered tiers has failed to produce a usable response. This reserves
// slow, low-throughput fallbacks like DNS tunneling for the case where the
// faster transports are blocked, rather than letting them compete on equal
// footing. When every transport shares the default priority (the common case),
// there is a single tier and behavior is unchanged.
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
	bodyBytes, err := drainRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("reading request body: %w", err)
	}

	eligible := t.filterTransports(req, bodyBytes)
	if len(eligible) == 0 {
		return nil, errors.New("no eligible transports for request")
	}

	ctx, cancel := context.WithTimeout(req.Context(), t.requestTimeout(req, eligible))
	defer cancel()

	idempotent := isRetryableMethod(req.Method) || req.Header.Get(IdempotentHeader) != ""
	tiers := groupByPriority(eligible)

	// Race each priority tier in turn. A tier that produces a usable response
	// (final) returns immediately; otherwise we hold its best fallback (a 5xx
	// response and/or the last error) and try the next tier. Slow last-resort
	// transports only get dialed once every faster tier has failed. heldResp /
	// heldErr carry the best fallback seen across all tiers so far.
	var heldResp *http.Response
	var heldErr error
	for i, tier := range tiers {
		// All transports in a tier share a priority, so the first reports it.
		// "tier" is the 0-based race order; "priority" is the Priority() value.
		t.log.Debug("Racing transport tier",
			"tier", i,
			"priority", priorityOf(tier[0]),
			"count", len(tier),
			"bodyLength", len(bodyBytes),
		)
		res := t.raceTier(ctx, req, tier, bodyBytes, idempotent)
		if res.final {
			drainAndClose(heldResp)
			return res.resp, res.err
		}
		// A 5xx held by this tier supersedes an earlier tier's fallback; an
		// empty resp leaves the earlier one in place.
		if res.resp != nil {
			drainAndClose(heldResp)
			heldResp = res.resp
		}
		if res.err != nil {
			heldErr = res.err
		}
		if ctx.Err() != nil {
			// The request's time budget is shared across tiers; once it's
			// spent (timeout or caller cancellation) there's no point dialing
			// another tier. Fall through to the best result held so far.
			break
		}
	}

	// All tiers exhausted (or the budget ran out) without a usable response.
	if heldResp != nil {
		return heldResp, nil
	}
	if ctx.Err() != nil {
		// The budget ran out before a later tier could be tried, so the
		// transports were not all exhausted. heldErr already conveys the
		// timeout (raceTier sets it to a "timed out, last error" wrap or
		// ctx.Err()); surface it directly rather than claiming every
		// transport failed.
		if heldErr != nil {
			return nil, heldErr
		}
		return nil, ctx.Err()
	}
	if heldErr != nil {
		return nil, fmt.Errorf("all transports failed: %w", heldErr)
	}
	return nil, errors.New("no transports produced a response")
}

// tierResult is the outcome of racing a single priority tier. When final is
// true, resp/err are exactly what RoundTrip should return — either a usable
// response or a single-shot non-idempotent result. When final is false the
// tier produced no usable response; resp holds the best fallback (a retryable
// 5xx) and err the last connection/request error, for RoundTrip to weigh
// against earlier tiers and carry into the next one. A timeout always reports
// final=false so RoundTrip can still surface a usable response held by an
// earlier tier; it stops iterating because the shared ctx is then done.
type tierResult struct {
	resp  *http.Response
	err   error
	final bool
}

// raceTier connects every transport in a single priority tier in parallel and
// applies the method-aware retry policy within that tier. See [raceTransport]
// for the retry semantics; the only addition is that an exhausted tier returns
// final=false so RoundTrip can advance to the next tier.
func (t *raceTransport) raceTier(ctx context.Context, req *http.Request, tier []Transport, bodyBytes []byte, idempotent bool) tierResult {
	// Each goroutine sends exactly one result, so the channel receives
	// len(tier) messages.
	results := make(chan connectResult, len(tier))
	addr := hostWithPort(req.URL.Host, req.URL.Scheme)
	for _, tr := range tier {
		go t.connect(ctx, tr, addr, results)
	}

	var heldResp *http.Response
	var heldErr error

	for remaining := len(tier); remaining > 0; remaining-- {
		select {
		case result := <-results:
			if result.err != nil {
				t.log.Error("Transport connection failed",
					"name", result.name,
					"error", result.err,
				)
				heldErr = result.err
				continue
			}

			t.log.Debug("Transport connected, sending request", "name", result.name, "method", req.Method)
			clone := cloneRequest(req, t.appName, result.name, bodyBytes)
			resp, err := result.rt.RoundTrip(clone)

			if !idempotent {
				// Single-shot: return whatever happened. Retrying on a non-
				// idempotent method risks replaying side effects.
				return tierResult{resp: resp, err: err, final: true}
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
				drainAndClose(resp)
				heldErr = err
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
				drainAndClose(heldResp)
				heldResp = resp
				heldErr = fmt.Errorf("transport %s: http status %d", result.name, resp.StatusCode)
				continue
			}

			// 2xx, 3xx, or 4xx on an idempotent method: 4xx is the server's
			// verdict on the request itself, retry won't help. Return.
			drainAndClose(heldResp)
			return tierResult{resp: resp, final: true}

		case <-ctx.Done():
			// Budget spent. Hand back whatever this tier held (if anything) as
			// non-final so RoundTrip can prefer it or an earlier tier's
			// fallback; RoundTrip stops iterating because ctx is now done.
			err := heldErr
			if err != nil {
				err = fmt.Errorf("timed out, last error: %w", err)
			} else if heldResp == nil {
				err = ctx.Err()
			}
			return tierResult{resp: heldResp, err: err}
		}
	}

	// Tier exhausted without a usable response; bubble up the best fallback so
	// RoundTrip can try the next tier (or return it if none succeed).
	return tierResult{resp: heldResp, err: heldErr}
}

// drainAndClose drains and closes a response body so the connection can be
// reused and nothing leaks. Safe to call with a nil response or nil body.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
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

// transportPriority is an optional interface a Transport may implement to
// influence race ordering. Transports are grouped into tiers by ascending
// priority, and a tier is raced only after every transport in all
// lower-numbered tiers has failed to produce a usable response. A transport
// that does not implement it is treated as priority 0 (raced first). Lower
// numbers race earlier; reserve higher numbers for slow fallbacks.
type transportPriority interface {
	Priority() int
}

// priorityOf reports a transport's race priority, defaulting to 0 for
// transports that do not implement transportPriority.
func priorityOf(tr Transport) int {
	if p, ok := tr.(transportPriority); ok {
		return p.Priority()
	}
	return priorityDefault
}

// groupByPriority splits transports into tiers ordered by ascending priority.
// Transports within a tier race in parallel; tiers are tried in order. Input
// order is preserved within each tier.
func groupByPriority(transports []Transport) [][]Transport {
	byPriority := make(map[int][]Transport)
	var priorities []int
	for _, tr := range transports {
		p := priorityOf(tr)
		if _, seen := byPriority[p]; !seen {
			priorities = append(priorities, p)
		}
		byPriority[p] = append(byPriority[p], tr)
	}
	sort.Ints(priorities)
	tiers := make([][]Transport, 0, len(priorities))
	for _, p := range priorities {
		tiers = append(tiers, byPriority[p])
	}
	return tiers
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

// requestTimeout returns the race budget for the request, using the
// longest timeout requested by any eligible transport (or the default
// if none overrides it). Only transports that survived filterTransports
// for this request influence the budget.
func (t *raceTransport) requestTimeout(req *http.Request, eligible []Transport) time.Duration {
	base := 80 * time.Second
	if req.Body != nil && req.Body != http.NoBody && req.ContentLength != 0 {
		base = 3 * time.Minute
	}
	for _, tr := range eligible {
		if tt := tr.RequestTimeout(); tt > base {
			base = tt
		}
	}
	return base
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
