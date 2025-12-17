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

func (n namedRoundTripper) maxLength() int64 {
	switch n.name {
	case "amp":
		// amp support requests with payloads lower than 6kb
		return 6000
	default:
		return -1
	}
}

func (t *raceTransport) RoundTrip(originalRequest *http.Request) (*http.Response, error) {
	// Try all methods in parallel and return the first successful response.
	// If all fail, return the last error.
	ctx, cancel := context.WithTimeout(originalRequest.Context(), timeout(originalRequest))

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
			t.connectedRoundTripper(ctx, d, originalRequest, errFunc, rtChan)
		}(d)
	}

	// Select up to the first response or error, or until we've hit the target number of tries or the context is canceled.
	retryTimes := len(t.roundTripperGenerators)
	var lastResponse *http.Response
	for range retryTimes {
		select {
		case rt := <-rtChan:
			// If we get a connection, try to send the request.
			log.Debug("Got connected RoundTripper", "name", rt.name)

			if originalRequest.ContentLength != -1 && rt.maxLength() != -1 &&
				originalRequest.ContentLength > rt.maxLength() {
				log.Debug("skipping transport because content length is bigger than maximum supported",
					slog.Int64("request-content-length", originalRequest.ContentLength),
					slog.Int64("max-transport-content-length", rt.maxLength()),
					slog.String("transport", rt.name),
				)
				continue
			}

			// Create a request with a cloned body to avoid issues with concurrent reads corrupting the body.
			req := cloneRequest(originalRequest, t.appName, rt.name)
			resp, err := rt.RoundTrip(req)
			if err != nil {
				log.Error("HTTP request failed", "name", rt.name, "err", err)
				errFunc(err)
				continue
			}
			// Treat all 2xx and 3xx responses as successful.
			if resp.StatusCode < http.StatusBadRequest {
				log.Debug("HTTP request succeeded", "name", rt.name, "status", resp.StatusCode)
				return resp, nil
			}
			// Given how many weird transports we're using underneath (i.e., it may be the intermediary transport
			// returning the response, not actually the destination server) we treat all other responses as retryable.
			log.Error("HTTP request returned retryable status", "name", rt.name, "status", resp.StatusCode)
			lastResponse = resp
			errFunc(fmt.Errorf("http status %d", resp.StatusCode))
		case err := <-errCh:
			log.Error("RoundTrip error", "error", err)
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if lastResponse != nil {
		return lastResponse, nil
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

func timeout(req *http.Request) time.Duration {
	// If there is no content length or it's zero, give a reduced timeout,
	// but not too short given that some transports can take awhile to
	// get set up.
	if req.ContentLength == -1 || req.ContentLength == 0 {
		return 80 * time.Second
	}

	// For larger uploads, give more time.
	return 3 * time.Minute
}
