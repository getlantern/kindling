package kindling

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type raceTransport struct {
	httpDialers   []httpDialer
	panicListener func(string)
}

func newRaceTransport(panicListener func(string), httpDialers ...httpDialer) http.RoundTripper {
	if panicListener == nil {
		panicListener = func(msg string) {
			log.Error(msg)
		}
	}
	return &raceTransport{
		httpDialers:   httpDialers,
		panicListener: panicListener,
	}
}

func (t *raceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	log.Debug("Starting RoundTrip race", "host", req.URL.Host)
	// Try all methods in parallel and return the first successful response.
	// If all fail, return the last error.
	ctx, cancel := context.WithTimeout(req.Context(), 1*time.Minute)

	// Note that this will cancel the context when the first response is received,
	// canceling any other in-flight requests that respect the context (which they should).
	defer cancel()
	var httpErrors = new(atomic.Int64)
	var roundTrippherCh = make(chan http.RoundTripper)
	var errCh = make(chan error)
	log.Debug(fmt.Sprintf("Dialing with %v dialers", len(t.httpDialers)))
	for _, d := range t.httpDialers {
		go func(d httpDialer) {
			// Recover from panics in the dialer.
			defer func() {
				if r := recover(); r != nil {
					t.panicListener(fmt.Sprintf("panic in dialer: %v", r))
					errCh <- fmt.Errorf("panic in dialer: %v", r)
				}
			}()
			t.connectedRoundTripper(ctx, d, req, errCh, roundTrippherCh, httpErrors)
		}(d)
	}
	// Select up to the first response or error, or until we've hit the target number of tries or the context is canceled.
	retryTimes := 3
	for i := 0; i < retryTimes; i++ {
		select {
		case roundTripper := <-roundTrippherCh:
			log.Debug("Got connected roundTripper", "host", req.URL.Host)
			// If we get a connection, try to send the request.
			resp, err := roundTripper.RoundTrip(req)
			// If the request fails, close the connection and return the error.
			if err != nil {
				log.Error("HTTP request failed", "err", err)
				continue
			}
			log.Debug("Got response:", "status", resp.Status, "host", req.URL.Host)
			return resp, nil
		case err := <-errCh:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("failed to get response")
}

func (t *raceTransport) connectedRoundTripper(ctx context.Context, d httpDialer, req *http.Request, errCh chan error, roundTrippherCh chan http.RoundTripper, httpErrors *atomic.Int64) {
	// We first create connected http.RoundTrippers prior to sending the request.
	// With this method, we don't have to worry about the idempotency of the request
	// because we ultimately try the connections serially in the next step.
	addr := req.URL.Host

	// The smart dialer requires the port to be specified, so we add it if it's
	// missing. We can't do this in the dialer itself because the scheme
	// is stripped by the time the dialer is called.
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if req.URL.Scheme == "https" {
			addr = net.JoinHostPort(addr, "443")
		} else {
			addr = net.JoinHostPort(addr, "80")
		}
	}
	log.Debug("Dialing", "addr", addr)
	connectedRoundTripper, err := d(ctx, addr)
	if err != nil {
		log.Error("Error dialing", "addr", addr, "err", err)
		if httpErrors.Add(1) == int64(len(t.httpDialers)) {
			errCh <- fmt.Errorf("failed to connect to any dialer with last error: %v", err)
		}
	} else {
		log.Debug("Dialing done", "addr", addr)
		roundTrippherCh <- connectedRoundTripper
	}
}
