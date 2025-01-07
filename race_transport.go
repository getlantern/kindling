package kindling

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"
)

type raceTransport struct {
	roundTrippers []http.RoundTripper
}

func newRaceTransport(roundTrippers ...http.RoundTripper) http.RoundTripper {
	return &raceTransport{
		roundTrippers: roundTrippers,
	}
}

func (t *raceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Try all RoundTrippers in parallel and return the first successful response.
	// If all fail, return the last error.
	ctx, cancel := context.WithTimeout(req.Context(), 1*time.Minute)
	defer cancel()
	var httpErrors atomic.Int64
	var respCh = make(chan *http.Response, 1)
	var errCh = make(chan error, 1)
	for _, rt := range t.roundTrippers {
		go func(rt http.RoundTripper, r *http.Request) {
			resp, err := rt.RoundTrip(r)
			if err != nil {
				if httpErrors.Add(1) == int64(len(t.roundTrippers)) {
					errCh <- err
				}
			} else {
				respCh <- resp
			}
		}(rt, req.Clone(ctx))
	}
	select {
	case resp := <-respCh:
		return resp, nil
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
