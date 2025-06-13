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
	roundTripperGenerators []roundTripperGenerator
	panicListener          func(string)
}

func newRaceTransport(panicListener func(string), roundTripperGenerators ...roundTripperGenerator) http.RoundTripper {
	if panicListener == nil {
		panicListener = func(msg string) {
			log.Error(msg)
		}
	}
	return &raceTransport{
		roundTripperGenerators: roundTripperGenerators,
		panicListener:          panicListener,
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
	var responseChan = make(chan *http.Response)
	var errCh = make(chan error)
	log.Debug(fmt.Sprintf("Dialing with %v dialers", len(t.roundTripperGenerators)))
	for _, d := range t.roundTripperGenerators {
		go func(d roundTripperGenerator) {
			// Recover from panics in the dialer.
			defer func() {
				if r := recover(); r != nil {
					t.panicListener(fmt.Sprintf("panic in dialer: %v", r))
					errCh <- fmt.Errorf("panic in dialer: %v", r)
				}
			}()
			t.connectedRoundTripper(ctx, d, req, errCh, responseChan, httpErrors)
		}(d)
	}
	// Select up to the first response or error, or until we've hit the target number of tries or the context is canceled.
	retryTimes := 3
	for range retryTimes {
		select {
		case resp := <-responseChan:
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

func (t *raceTransport) connectedRoundTripper(ctx context.Context, d roundTripperGenerator, req *http.Request, errCh chan error, responseChan chan *http.Response, httpErrors *atomic.Int64) {
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
	errFunc := func(err error) {
		log.Error("Error dialing", "addr", addr, "err", err)
		if httpErrors.Add(1) == int64(len(t.roundTripperGenerators)) {
			errCh <- fmt.Errorf("%v failed to connect to any dialer with last error: %v", d.name(), err)
		}
	}
	log.Debug("Dialing", "addr", addr)
	connectedRoundTripper, err := d.roundTripper(ctx, addr)
	if err != nil {
		errFunc(err)
	} else {
		log.Debug("Dialing done", "addr", addr)
		log.Debug("Got connected roundTripper", "host", req.URL.Host)
		select {
		case <-ctx.Done():
			// context is canceled - we should not proceed with the request
			log.Debug("Context canceled before sending request", "host", req.URL.Host)
			return
		default:
			// context is not canceled
		}
		// If we get a connection, try to send the request.
		resp, err := connectedRoundTripper.RoundTrip(req)
		if err != nil {
			log.Error("HTTP request failed", "err", err)
			errFunc(err)
			return
		}
		select {
		case responseChan <- resp:
			// sent successfully
		case <-ctx.Done():
			// context canceled, close response to avoid leak
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
		}
	}
}
