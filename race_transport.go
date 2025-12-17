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
	transports    []Transport
	panicListener func(string)
	appName       string
}

func newRaceTransport(appName string, panicListener func(string), transports ...Transport) http.RoundTripper {
	if panicListener == nil {
		panicListener = func(msg string) {
			log.Error(msg)
		}
	}
	return &raceTransport{
		transports:    transports,
		panicListener: panicListener,
		appName:       appName,
	}
}

type namedRoundTripper struct {
	http.RoundTripper
	name string
}

func (t *raceTransport) RoundTrip(originalRequest *http.Request) (*http.Response, error) {
	// Try all methods in parallel and return the first successful response.
	// If all fail, return the last error.
	ctx, cancel := context.WithTimeout(originalRequest.Context(), timeout(originalRequest))

	// Note that this will cancel the context when the first response is received,
	// canceling any other in-flight requests that respect the context (which they should).
	defer cancel()
	var httpErrors = new(atomic.Int64)
	var rtChan = make(chan *namedRoundTripper, len(t.transports))
	var errCh = make(chan error, len(t.transports))
	errFunc := func(err error) {
		if httpErrors.Add(1) == int64(len(t.transports)) {
			errCh <- fmt.Errorf("failed to connect to any dialer with last error: %v", err)
		}
	}
	// Store a raw copy of the request body for request copies sent to the various
	// transports.
	bodyBytes := requestBodyBytes(originalRequest)
	log.Debug(fmt.Sprintf("Dialing with %v dialers and body length %v", len(t.transports), len(bodyBytes)))
	for _, tr := range t.transports {
		hasLimit := tr.MaxLength() > 0
		if hasLimit && len(bodyBytes) > tr.MaxLength() {
			log.Debug("Skipping transport due to size limit", "name", tr.Name(), "size", len(bodyBytes), "maxLength", tr.MaxLength())
			continue
		}
		go func(tr Transport) {
			// Recover from panics in the dialer.
			defer func() {
				if r := recover(); r != nil {
					t.panicListener(fmt.Sprintf("panic in dialer: %v", r))
					errCh <- fmt.Errorf("panic in dialer: %v", r)
				}
			}()
			t.connectedRoundTripper(ctx, tr, originalRequest, errFunc, rtChan)
		}(tr)
	}

	// Select up to the first response or error, or until we've hit the target number of tries or the context is canceled.
	retryTimes := len(t.transports)
	var lastResponse *http.Response
	for range retryTimes {
		select {
		case rt := <-rtChan:
			// If we get a connection, try to send the request.
			log.Debug("Got connected RoundTripper", "name", rt.name)

			// Create a request with a cloned body to avoid issues with concurrent reads corrupting the body.
			req := cloneRequest(originalRequest, t.appName, rt.name, bodyBytes)
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

func (t *raceTransport) connectedRoundTripper(ctx context.Context, tr Transport, originalReq *http.Request, errFunc func(error), rtChan chan *namedRoundTripper) {
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

	connectedRoundTripper, err := tr.NewRoundTripper(ctx, addr)
	if err != nil {
		errFunc(err)
	} else {
		if ctx.Err() != nil {
			// context is canceled - we should not proceed with the request
			log.Debug("Context canceled before sending request", "host", originalReq.URL.Host)
			errFunc(ctx.Err())
			return
		}
		rtChan <- &namedRoundTripper{RoundTripper: connectedRoundTripper, name: tr.Name()}
	}
}

// Protect the http request with a mutex to avoid concurrent reads.
var reqMutex = new(sync.Mutex)

// cloneRequest creates a copy of the provided HTTP request, including its body.
// If the body is nil or http.NoBody, it simply returns a clone without reading the body.
// This is important because, since we're racing requests, it's possible that the body
// has been consumed by a previous request.
func cloneRequest(req *http.Request, app, method string, bodyBytes []byte) *http.Request {
	reqMutex.Lock()
	defer reqMutex.Unlock()
	clonedReq := req.Clone(req.Context())
	clonedReq.Header.Add("X-Kindling-App", app)
	clonedReq.Header.Add("X-Kindling-Method", method)
	if req.Body == http.NoBody || req.Body == nil {
		// If the request body is nil, we can just return a clone without reading it.
		return clonedReq
	}
	clonedReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return clonedReq
}

func timeout(req *http.Request) time.Duration {
	cl := req.Header.Get("Content-Length")

	// If there is no content length or it's zero, give a reduced timeout,
	// but not too short given that some transports can take awhile to
	// get set up.
	if cl == "" || cl == "0" {
		return 80 * time.Second
	}

	// For larger uploads, give more time.
	return 3 * time.Minute
}

func requestBodyBytes(req *http.Request) []byte {
	if req.Body == nil || req.Body == http.NoBody {
		return []byte{}
	}
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		log.Error("Error reading request body:", "error", err)
		return []byte{}
	}
	// Restore the original request body for future reads.
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return bodyBytes
}
