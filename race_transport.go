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
// that it normally reserves for GET/HEAD: transport-level errors, 5xx
// and 403 responses fall back to the next connected transport.
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
//     behavior is preserved: transport-level errors after RoundTrip, 5xx and
//     403 responses fall back to the next connected transport. This handles
//     the case where an intermediary (rather than the origin) produced the
//     response — common when one fronting CDN is being blocked, or when a
//     front such as GFE refuses the client's country on one path only.
//     Other 4xx responses are NOT retried even for idempotent methods: they
//     are the server's verdict on the request itself, so retrying won't help.
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
// Transports are grouped into priority tiers (see [transportPriority]) and
// may carry a start delay (see [transportStartDelay]). Default-tier
// transports without a delay race in parallel as described above. A transport
// with a delay joins the race once its delay has passed, and a later tier
// joins once every transport already racing has failed to produce a usable
// response, whichever comes first. Transports already racing keep going.
// This keeps slow, low-throughput fallbacks like DNS tunneling out of races
// the faster transports win quickly, while still bringing them in promptly
// when the faster ones hang rather than fail, as they do on networks that
// drop packets silently. When every transport shares the default priority
// and has no delay (the common case), all of them race at once.
type raceTransport struct {
	transports    []Transport
	panicListener func(string)
	appName       string
	log           *slog.Logger
	observer      func(context.Context, Race)
	// startDelays overrides transports' own start delays, by name.
	startDelays map[string]time.Duration
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
	id   int       // race position, for the observer
	at   time.Time // when the outcome happened
	err  error
}

func (t *raceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var rec *raceRecord
	if t.observer != nil {
		rec = newRaceRecord(req.Context())
		defer func() { t.observer(req.Context(), rec.finish()) }()
	}
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
	res := t.race(ctx, req, eligible, bodyBytes, idempotent, rec)
	if res.final {
		if res.resp != nil && res.err == nil {
			rec.won(res.id)
		}
		return res.resp, res.err
	}

	// Every transport was tried (or the budget ran out) without a usable response.
	if res.resp != nil {
		rec.won(res.id)
		return res.resp, nil
	}
	if ctx.Err() != nil {
		// The budget ran out before every transport could be tried, so they
		// were not all exhausted. res.err already conveys the timeout (race
		// sets it to a "timed out, transport errors" wrap or ctx.Err());
		// surface it directly rather than claiming every transport failed.
		if res.err != nil {
			return nil, res.err
		}
		return nil, ctx.Err()
	}
	if res.err != nil {
		return nil, fmt.Errorf("all transports failed: %w", res.err)
	}
	return nil, errors.New("no transports produced a response")
}

// raceResult is the outcome of a race. When final is true, resp/err are
// exactly what RoundTrip should return — either a usable response or a
// single-shot non-idempotent result. When final is false no transport
// produced a usable response; resp holds the best fallback (a retryable 5xx
// or 403) and err every connection/request error joined. A timeout always
// reports final=false so RoundTrip can still return a held response.
type raceResult struct {
	resp  *http.Response
	id    int // race position of the transport that produced resp
	err   error
	final bool
}

// pendingTransport is one eligible transport's place in a race.
type pendingTransport struct {
	tr      Transport
	tier    int
	delay   time.Duration
	started bool
}

// sent is the outcome of sending the request through one connection.
type sent struct {
	result connectResult
	resp   *http.Response
	err    error
}

// race connects the eligible transports and applies the method-aware retry
// policy (see [raceTransport]) across all of them. A default-tier transport
// with no start delay connects at once. Every other transport starts when its
// delay elapses, or as soon as every transport started so far has failed to
// produce a usable response, whichever comes first. In the second case the
// whole lowest tier still waiting starts together. A transport in a later tier
// with no delay waits for that. Transports already running keep going when
// another starts, so whichever produces a usable response first wins.
//
// Requests go out one at a time: a connection that arrives while a request is
// in flight waits its turn. The exception is a replay-safe request once any
// transport has joined on its start delay. The delay exists to rescue
// attempts that hang, and a request that connected but never answers is one,
// so from then on every connection sends as soon as it's up, alongside
// whatever is in flight rather than behind it.
func (t *raceTransport) race(ctx context.Context, req *http.Request, eligible []Transport, bodyBytes []byte, idempotent bool, rec *raceRecord) raceResult {
	begin := time.Now()
	// Each started transport sends exactly one connect result and at most one
	// request, so neither channel ever holds more than len(eligible).
	results := make(chan connectResult, len(eligible))
	sends := make(chan sent, len(eligible))
	addr := hostWithPort(req.URL.Host, req.URL.Scheme)

	var pending []*pendingTransport
	for i, tier := range groupByPriority(eligible) {
		for _, tr := range tier {
			pending = append(pending, &pendingTransport{tr: tr, tier: i, delay: t.startDelayOf(tr)})
		}
	}
	running, nextID := 0, 0
	start := func(p *pendingTransport, reason string) {
		p.started = true
		running++
		id := nextID
		nextID++
		rec.racing([]Transport{p.tr})
		t.log.Debug("Starting transport",
			"name", p.tr.Name(),
			"reason", reason,
			"priority", priorityOf(p.tr),
			"after", time.Since(begin),
			"bodyLength", len(bodyBytes),
		)
		go t.connect(ctx, p.tr, id, addr, results)
	}
	for _, p := range pending {
		if p.tier == 0 && p.delay == 0 {
			start(p, "first")
		}
	}

	// Each request gets its own context, so a request still in flight when
	// another wins can be cancelled without touching the winner's body.
	cancels := map[int]context.CancelFunc{}
	inFlight := 0
	var queue []connectResult
	// A connection the race never sends through may hold a pre-dialed conn or
	// tunnel stream, so release it: the queued ones now, and any still
	// connecting once they report.
	defer func() {
		for _, r := range queue {
			discard(r.rt)
		}
		if running > 0 {
			go func(n int) {
				for range n {
					discard((<-results).rt)
				}
			}(running)
		}
	}()
	// drainReady records connection failures that already arrived when the
	// race ends, so they aren't reported as unfinished. It doesn't wait for
	// the rest.
	drainReady := func() {
		for {
			select {
			case r := <-results:
				running--
				if r.err != nil {
					rec.connectFailed(ctx, r.id, r.err, r.at)
				}
				discard(r.rt)
			default:
				return
			}
		}
	}
	// hedging is set once a transport joins on its delay; see above.
	hedging := false
	send := func(result connectResult) {
		t.log.Debug("Transport connected, sending request", "name", result.name, "method", req.Method)
		sendCtx, cancel := context.WithCancel(req.Context())
		cancels[result.id] = cancel
		clone := cloneRequest(req.WithContext(sendCtx), t.appName, result.name, bodyBytes)
		inFlight++
		go func() {
			defer func() {
				if r := recover(); r != nil {
					msg := fmt.Sprintf("panic in transport %s: %v", result.name, r)
					t.panicListener(msg)
					sends <- sent{result: result, err: errors.New(msg)}
				}
			}()
			resp, err := result.rt.RoundTrip(clone)
			sends <- sent{result: result, resp: resp, err: err}
		}()
	}
	// finish cancels every request but keep's, and closes the responses of
	// any still in flight once they arrive.
	finish := func(keep int) {
		for id, cancel := range cancels {
			if id != keep {
				cancel()
			}
		}
		if inFlight > 0 {
			go func(n int) {
				for range n {
					// Closed, not drained: the request is cancelled, and an
					// upgraded connection would never reach EOF.
					if resp := (<-sends).resp; resp != nil && resp.Body != nil {
						_ = resp.Body.Close()
					}
				}
			}(inFlight)
		}
	}

	// keep hands resp to the caller. Its body outlives the race, so its
	// request's context is cancelled when the caller closes it; with no
	// response, at once.
	keep := func(resp *http.Response, id int) {
		cancel, ok := cancels[id]
		if !ok {
			return
		}
		if resp == nil || resp.Body == nil {
			cancel()
			return
		}
		body := &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		if w, ok := resp.Body.(io.Writer); ok {
			// A 101 Switching Protocols body is the upgraded connection,
			// which callers write to as well.
			resp.Body = &cancelOnCloseWriter{cancelOnClose: body, Writer: w}
			return
		}
		resp.Body = body
	}

	var heldResp *http.Response
	heldID := -1
	var heldErr error

	// timedOut hands back whatever the race held as non-final, so RoundTrip
	// can still return a held response.
	timedOut := func() raceResult {
		// Drain first. select chooses uniformly when several cases are
		// ready, so a connectResult that has already landed can be passed
		// over in favour of ctx.Done() and its error lost — which defeats
		// the errors.Is guarantee precisely when a transport failed fast
		// and the budget expired at about the same moment.
		//
		// Non-blocking, and deliberately no RoundTrip on anything drained:
		// the budget is gone, so a transport that connected is not worth
		// sending a request through. Only its failure, if any, is news.
	drain:
		for {
			select {
			case r := <-results:
				running--
				if r.err != nil {
					heldErr = errors.Join(heldErr, fmt.Errorf("transport %s: %w", r.name, r.err))
					rec.connectFailed(ctx, r.id, r.err, r.at)
				}
				discard(r.rt)
			case s := <-sends:
				// A request that answered just as the budget ran out still
				// counts: its response may be the usable one.
				inFlight--
				if res, final := t.settle(s, req.Method, idempotent, &heldResp, &heldID, &heldErr, cancels, rec); final {
					keep(res.resp, res.id)
					finish(res.id)
					drainReady()
					return res
				}
			default:
				break drain
			}
		}
		keep(heldResp, heldID)
		finish(heldID)

		err := heldErr
		if err != nil {
			// Not "last error" any more: heldErr carries every failure the
			// race collected, so naming one would misdescribe the rest.
			err = fmt.Errorf("timed out, transport errors: %w", err)
		} else if heldResp == nil {
			err = ctx.Err()
		}
		return raceResult{resp: heldResp, id: heldID, err: err}
	}

	for {
		if running == 0 && inFlight == 0 && len(queue) == 0 {
			if ctx.Err() != nil {
				return timedOut()
			}
			// Everything started so far has failed: start the lowest tier
			// that still has transports waiting, without waiting out their
			// delays.
			next := -1
			for _, p := range pending {
				if !p.started && (next < 0 || p.tier < next) {
					next = p.tier
				}
			}
			if next < 0 {
				// Every transport was tried without a usable response;
				// bubble up the best fallback.
				keep(heldResp, heldID)
				finish(heldID)
				return raceResult{resp: heldResp, id: heldID, err: heldErr}
			}
			for _, p := range pending {
				if !p.started && p.tier == next {
					start(p, "others failed")
				}
			}
		}

		var timer *time.Timer
		var due <-chan time.Time
		if d, ok := nextStart(pending); ok {
			timer = time.NewTimer(d - time.Since(begin))
			due = timer.C
		}

		select {
		case result := <-results:
			running--
			if result.err != nil {
				t.log.Error("Transport connection failed",
					"name", result.name,
					"error", result.err,
				)
				// Joined, not assigned, and named. Transports race
				// concurrently, so assigning kept only whichever lost last and
				// silently dropped its siblings — and the name lived in
				// connectResult rather than in the error, so even the survivor
				// was unattributable unless its own text happened to say which
				// strategy produced it.
				heldErr = errors.Join(heldErr, fmt.Errorf("transport %s: %w", result.name, result.err))
				rec.connectFailed(ctx, result.id, result.err, result.at)
			} else if inFlight == 0 || (idempotent && hedging) {
				send(result)
			} else {
				queue = append(queue, result)
			}

		case s := <-sends:
			inFlight--
			if res, final := t.settle(s, req.Method, idempotent, &heldResp, &heldID, &heldErr, cancels, rec); final {
				keep(res.resp, res.id)
				finish(res.id)
				drainReady()
				return res
			}
			if inFlight == 0 && len(queue) > 0 {
				next := queue[0]
				queue = queue[1:]
				send(next)
			}

		case <-due:
			elapsed := time.Since(begin)
			for _, p := range pending {
				if !p.started && p.delay > 0 && p.delay <= elapsed {
					hedging = true
					start(p, "delay elapsed")
				}
			}
			if hedging && idempotent {
				for _, r := range queue {
					send(r)
				}
				queue = nil
			}

		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return timedOut()
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// settle applies the retry policy to one request's outcome. It returns
// final=true with what the race should return, or holds a retryable response
// and records the failure so the race goes on.
func (t *raceTransport) settle(s sent, method string, idempotent bool, heldResp **http.Response, heldID *int, heldErr *error, cancels map[int]context.CancelFunc, rec *raceRecord) (raceResult, bool) {
	result, resp, err := s.result, s.resp, s.err
	if !idempotent {
		// Single-shot: return whatever happened. Retrying on a non-
		// idempotent method risks replaying side effects.
		//
		// Named here too, because this branch returns before the
		// attribution below and POST is exactly where it matters: the
		// registration call that motivated naming these errors is a
		// POST, so without this the one path under investigation was
		// the one path still returning an unattributed error.
		if err != nil {
			rec.requestFailed(result.id, statusOf(resp), err)
			err = fmt.Errorf("transport %s: %w", result.name, err)
		} else {
			rec.add(result.id, OutcomeWon, resp.StatusCode, nil)
		}
		return raceResult{resp: resp, id: result.id, err: err, final: true}, true
	}

	if err != nil {
		t.log.Warn("HTTP request failed on idempotent method, falling back",
			"name", result.name,
			"method", method,
			"error", err,
		)
		// http.RoundTripper allows resp to be non-nil even when
		// err is non-nil (the spec only requires resp == nil when
		// err == nil for the success direction). Drain + close
		// defensively so we don't leak the body / connection.
		rec.requestFailed(result.id, statusOf(resp), err)
		cancels[result.id]()
		drainAndClose(resp)
		*heldErr = errors.Join(*heldErr, fmt.Errorf("transport %s: %w", result.name, err))
		return raceResult{}, false
	}

	if isRetryableStatus(resp.StatusCode) {
		// The response may be from a blocked intermediary rather than
		// the origin. Try the next transport. Hold this response in
		// case nothing else works.
		t.log.Warn("Retryable status on idempotent method, falling back",
			"name", result.name,
			"method", method,
			"status", resp.StatusCode,
		)
		if *heldResp != nil {
			// Cancelled first, so a held body that stalls can't block the
			// drain.
			cancels[*heldID]()
			drainAndClose(*heldResp)
		}
		*heldResp, *heldID = resp, result.id
		*heldErr = errors.Join(*heldErr, fmt.Errorf("transport %s: http status %d", result.name, resp.StatusCode))
		rec.add(result.id, OutcomeRetryStatus, resp.StatusCode, nil)
		return raceResult{}, false
	}

	// 2xx, 3xx, or any other 4xx on an idempotent method: that 4xx is
	// the server's verdict on the request itself, retry won't help.
	if *heldResp != nil {
		cancels[*heldID]()
		drainAndClose(*heldResp)
		*heldResp, *heldID = nil, -1
	}
	rec.add(result.id, OutcomeWon, resp.StatusCode, nil)
	return raceResult{resp: resp, id: result.id, final: true}, true
}

// cancelOnClose cancels a request's context once its response body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// cancelOnCloseWriter is a cancelOnClose for a body that's also writable.
type cancelOnCloseWriter struct {
	*cancelOnClose
	io.Writer
}

// nextStart returns the earliest start delay among transports still waiting,
// if any is waiting on one.
func nextStart(pending []*pendingTransport) (time.Duration, bool) {
	var next time.Duration
	found := false
	for _, p := range pending {
		if !p.started && p.delay > 0 && (!found || p.delay < next) {
			next, found = p.delay, true
		}
	}
	return next, found
}

// discard releases a connected round tripper the race won't send through.
// Some hold a pre-dialed connection (domain fronting) or tunnel stream (Soar)
// that only Close gives back.
func discard(rt http.RoundTripper) {
	if c, ok := rt.(io.Closer); ok {
		_ = c.Close()
	}
}

// statusOf returns resp's status code, or 0 without a response.
func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
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

// isRetryableStatus reports whether an idempotent request should fall back to
// another transport on this status. 403 is included because intermediaries
// return it for path-specific blocks, e.g. Google Front End refusing a
// sanctioned country on the direct route while a fronted route would succeed.
func isRetryableStatus(code int) bool {
	return code >= 500 || code == http.StatusForbidden
}

// isRetryableMethod reports whether requests with this method are safe to
// replay on a different transport after a transport-level error or
// retryable-status response. Only GET and HEAD are included: they have no side effects
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
func (t *raceTransport) connect(ctx context.Context, tr Transport, id int, addr string, results chan<- connectResult) {
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("panic in transport %s: %v", tr.Name(), r)
			t.panicListener(msg)
			results <- connectResult{name: tr.Name(), id: id, at: time.Now(), err: errors.New(msg)}
		}
	}()

	rt, err := tr.NewRoundTripper(ctx, addr)
	if err != nil {
		results <- connectResult{name: tr.Name(), id: id, at: time.Now(), err: err}
		return
	}
	if ctx.Err() != nil {
		discard(rt)
		results <- connectResult{name: tr.Name(), id: id, at: time.Now(), err: ctx.Err()}
		return
	}
	results <- connectResult{rt: rt, name: tr.Name(), id: id, at: time.Now()}
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

// transportStartDelay is an optional interface a Transport may implement to
// join the race late. A transport with a delay starts that long into the
// request, or sooner if every transport already racing fails first. Zero, the
// default, means a default-tier transport starts at once and a later-tier one
// waits for the tiers before it to fail.
type transportStartDelay interface {
	StartDelay() time.Duration
}

// startDelayOf reports a transport's start delay: the one set by
// [WithStartDelay] if any, else its own, else zero.
func (t *raceTransport) startDelayOf(tr Transport) time.Duration {
	if d, ok := t.startDelays[tr.Name()]; ok {
		return d
	}
	return startDelayOf(tr)
}

func startDelayOf(tr Transport) time.Duration {
	if d, ok := tr.(transportStartDelay); ok {
		return d.StartDelay()
	}
	return 0
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
	if req.Body != nil && req.Body != http.NoBody {
		// Every attempt gets its own reader, even for an empty body: hedged
		// attempts read concurrently.
		clone.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		if len(bodyBytes) > 0 {
			clone.ContentLength = int64(len(bodyBytes))
		}
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
