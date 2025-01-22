package kindling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

type raceTransport struct {
	httpDialers []httpDialer
}

func newRaceTransport(httpDialers ...httpDialer) http.RoundTripper {
	return &raceTransport{
		httpDialers: httpDialers,
	}
}

func (t *raceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Try all methods in parallel and return the first successful response.
	// If all fail, return the last error.
	ctx, cancel := context.WithTimeout(req.Context(), 1*time.Minute)

	// Note that this will cancel the context when the first response is received,
	// canceling any other in-flight requests that respect the context (which they should).
	defer cancel()
	var httpErrors atomic.Int64
	var roundTrippherCh = make(chan http.RoundTripper)
	var errCh = make(chan error)
	for _, d := range t.httpDialers {
		go func(d httpDialer) {
			// We first create connected http.RoundTrippers prior to sending the request.
			// With this method, we don't have to worry about the idempotency of the request
			// because we ultimately try the connections serially in the next step.
			connectedRoundTripper, err := d(ctx, req.URL.Host)
			if err != nil {
				if httpErrors.Add(1) == int64(len(t.httpDialers)) {
					errCh <- fmt.Errorf("failed to connect to any dialer with last error: %v", err)
				}
			} else {
				roundTrippherCh <- connectedRoundTripper
			}
		}(d)
	}
	// Select up to the first response or error, or until we've hit the target number of tries or the context is canceled.
	retryTimes := 3
	for i := 0; i < retryTimes; i++ {
		select {
		case roundTripper := <-roundTrippherCh:
			// If we get a connection, try to send the request.
			resp, err := roundTripper.RoundTrip(req)
			// If the request fails, close the connection and return the error.
			if err != nil {
				slog.Error("HTTP request failed", "error", err)
				continue
			}

			cancel()
			return resp, nil
		case err := <-errCh:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("failed to get response")
}
