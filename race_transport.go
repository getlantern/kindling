package kindling

import (
	"errors"
	"net/http"
	"sync/atomic"
	"time"
)

type raceTransport struct {
	fronted        http.RoundTripper
	smartTransport http.RoundTripper
}

func (t *raceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Try both fronted and smart dialer in parallel.
	frontedCh := make(chan *http.Response, 1)
	smartCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := t.fronted.RoundTrip(req)
		if err != nil {
			errCh <- err
		} else {
			frontedCh <- resp
		}
	}()
	go func() {
		resp, err := t.smartTransport.RoundTrip(req)
		if err != nil {
			errCh <- err
		} else {
			smartCh <- resp
		}
	}()

	var httpErrors atomic.Int32
	// Wait for the first one to succeed.
	select {
	case resp := <-frontedCh:
		return resp, nil
	case resp := <-smartCh:
		return resp, nil
	case err := <-errCh:
		if httpErrors.Add(1) == 2 {
			return nil, err
		}
	case <-time.After(1 * time.Minute):
		return nil, http.ErrHandlerTimeout
	}
	return nil, errors.New("unreachable")
}
