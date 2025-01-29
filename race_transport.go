package kindling

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var tracer = otel.Tracer("kindling")

type raceTransport struct {
	httpDialers []httpDialer
}

func newRaceTransport(httpDialers ...httpDialer) http.RoundTripper {
	return &raceTransport{
		httpDialers: httpDialers,
	}
}

func (t *raceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := tracer.Start(req.Context(), "RoundTrip")
	defer span.End()

	span.SetAttributes(attribute.String("http.url", req.URL.String()))

	log.Debugf("Starting RoundTrip race %v", req.URL.Host)
	// Try all methods in parallel and return the first successful response.
	// If all fail, return the last error.
	ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)

	// Note that this will cancel the context when the first response is received,
	// canceling any other in-flight requests that respect the context (which they should).
	defer cancel()
	var httpErrors = new(atomic.Int64)
	var roundTrippherCh = make(chan http.RoundTripper)
	var errCh = make(chan error)
	log.Debugf("Dialing with %v dialers", len(t.httpDialers))
	for _, d := range t.httpDialers {
		go func(d httpDialer) {
			t.connectedRoundTripper(ctx, d, req, errCh, roundTrippherCh, cancel, httpErrors)
		}(d)
	}
	// Create a span for the request/response cycle.
	reqRespCtx, reqRespSpan := tracer.Start(ctx, "RequestResponse")
	defer reqRespSpan.End()
	// Select up to the first response or error, or until we've hit the target number of tries or the context is canceled.
	retryTimes := 3
	for i := 0; i < retryTimes; i++ {
		select {
		case roundTripper := <-roundTrippherCh:
			log.Debugf("Got connected roundTripper for %v", req.URL.Host)
			// Since we're already connected, set a lower timeout on the request context.
			singleRTCtx, cancelRoundTrip := context.WithTimeout(reqRespCtx, 10*time.Second)
			req = req.Clone(singleRTCtx)

			// If we get a connection, try to send the request.
			resp, err := roundTripper.RoundTrip(req)
			// If the request fails, close the connection and return the error.
			if err != nil {
				log.Errorf("HTTP request failed %v", err)
				cancelRoundTrip()
				continue
			}
			log.Debugf("Got response '%v' for %v", resp.Status, req.URL.Host)
			cancelRoundTrip()
			return resp, nil
		case err := <-errCh:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("failed to get response")
}

func (t *raceTransport) connectedRoundTripper(parentCtx context.Context, d httpDialer, req *http.Request, errCh chan error, roundTrippherCh chan http.RoundTripper, cancel context.CancelFunc, httpErrors *atomic.Int64) {
	dialCtx, dialSpan := tracer.Start(parentCtx, "Dial")
	defer dialSpan.End()

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
	log.Debugf("Dialing %v", addr)
	connectedRoundTripper, err := d(dialCtx, addr)
	if err != nil {
		log.Errorf("Error dialing to %v:\n%v", addr, err)
		if httpErrors.Add(1) == int64(len(t.httpDialers)) {
			errCh <- fmt.Errorf("failed to connect to any dialer with last error: %v", err)
		}
	} else {
		log.Debugf("Dialing done %v", addr)
		roundTrippherCh <- connectedRoundTripper
	}
}
