package kindling

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type raceTransport struct {
	roundTripperGenerators []roundTripperGenerator
	panicListener          func(string)
	appName                string
}

func newRaceTransport(appName string, panicListener func(string), roundTripperGenerators ...roundTripperGenerator) http.RoundTripper {
	if panicListener == nil {
		panicListener = func(msg string) {
			log.Error(msg)
		}
	}
	return &raceTransport{
		roundTripperGenerators: roundTripperGenerators,
		panicListener:          panicListener,
		appName:                appName,
	}
}

type namedRoundTripper struct {
	http.RoundTripper
	name string
}

func (t *raceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Try all methods in parallel and return the first successful response.
	// If all fail, return the last error.
	ctx, cancel := context.WithTimeout(req.Context(), 1*time.Minute)

	// Note that this will cancel the context when the first response is received,
	// canceling any other in-flight requests that respect the context (which they should).
	defer cancel()
	var httpErrors = new(atomic.Int64)
	var rtChan = make(chan *namedRoundTripper, len(t.roundTripperGenerators))
	var errCh = make(chan error, len(t.roundTripperGenerators))
	errFunc := func(err error) {
		if httpErrors.Add(1) == int64(len(t.roundTripperGenerators)) {
			errCh <- fmt.Errorf("failed to connect to any dialer with last error: %v", err)
		}
	}
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
			t.connectedRoundTripper(ctx, d, req, errFunc, rtChan)
		}(d)
	}

	// Select up to the first response or error, or until we've hit the target number of tries or the context is canceled.
	retryTimes := len(t.roundTripperGenerators)
	for range retryTimes {
		select {
		case rt := <-rtChan:
			// If we get a connection, try to send the request.
			log.Debug("Got connected RoundTripper", "name", rt.name)

			// Create a request with a cloned body to avoid issues with concurrent reads and also
			// set a lower timeout for the actual request so any single request doesn't use our whole
			// allotted time.
			requestCtx, requestCancel := context.WithTimeout(ctx, 10*time.Second)
			req = cloneRequest(req, t.appName, rt.name).WithContext(requestCtx)
			resp, err := rt.RoundTrip(req)
			if err != nil {
				log.Error("HTTP request failed", "name", rt.name, "err", err)
				errFunc(err)
				requestCancel()
				continue
			}
			log.Info("HTTP request succeeded", "name", rt.name, "status", resp.StatusCode)
			requestCancel()
			return resp, nil
		case err := <-errCh:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("failed to get response")
}

func (t *raceTransport) connectedRoundTripper(ctx context.Context, d roundTripperGenerator, originalReq *http.Request, errFunc func(error), rtChan chan *namedRoundTripper) {
	// We first create connected http.RoundTrippers prior to sending the request.
	// With this method, we don't have to worry about the idempotency of the request
	// because we ultimately try the connections serially in the next step.
	addr := originalReq.URL.Host

	// The smart dialer requires the port to be specified, so we add it if it's
	// missing. We can't do this in the dialer itself because the scheme
	// is stripped by the time the dialer is called.
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if originalReq.URL.Scheme == "https" {
			addr = net.JoinHostPort(addr, "443")
		} else {
			addr = net.JoinHostPort(addr, "80")
		}
	}

	connectedRoundTripper, err := d.roundTripper(ctx, addr)
	if err != nil {
		errFunc(err)
	} else {
		if ctx.Err() != nil {
			// context is canceled - we should not proceed with the request
			log.Debug("Context canceled before sending request", "host", originalReq.URL.Host)
			errFunc(ctx.Err())
			return
		}
		rtChan <- &namedRoundTripper{RoundTripper: connectedRoundTripper, name: d.name()}
	}
}

// Protect the http request with a mutex to avoid concurrent reads.
var reqMutex = new(sync.Mutex)

// cloneRequest creates a copy of the provided HTTP request, including its body.
// If the body is nil or http.NoBody, it simply returns a clone without reading the body.
// This is important because, since we're racing requests, it's possible that the body
// has been consumed by a previous request.
func cloneRequest(req *http.Request, app, method string) *http.Request {
	reqMutex.Lock()
	defer reqMutex.Unlock()
	clonedReq := req.Clone(req.Context())
	clonedReq.Header.Add("X-Kindling-App", app)
	clonedReq.Header.Add("X-Kindling-Method", method)
	if req.Body == http.NoBody || req.Body == nil {
		// If the request body is nil, we can just return a clone without reading it.
		return clonedReq
	}
	// Read the original body into a buffer
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		log.Error("Error reading body:", "error", err)
		return req
	}
	req.Body.Close() // Close the original body

	// Replace the bodies with new readers from the buffer
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	clonedReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return clonedReq
}
