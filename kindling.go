package kindling

import (
	"context"
	"embed"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/x/smart"
	"github.com/getlantern/fronted"
)

var log = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{}))

// Kindling is the interface that wraps the basic Dial and DialContext methods for control
// plane traffic.
type Kindling interface {
	// NewHTTPClient returns a new HTTP client that is configured to use kindling.
	NewHTTPClient() *http.Client
}

type httpDialer func(ctx context.Context, addr string) (http.RoundTripper, error)

type kindling struct {
	httpDialers   []httpDialer
	logWriter     io.Writer
	panicListener func(string)
}

// Make sure that kindling implements the Kindling interface.
var _ Kindling = &kindling{}

// Create an enum of priority levels for options where the priority matters (things like panic listeners
// that need to be set before other options).
const (
	priorityLogWriter = iota
	priorityPanicListener
)

// Option is a functional option type that allows us to configure the Client.
type Option interface {
	apply(*kindling)
	priority() int
}

// NewKindling returns a new Kindling.
func NewKindling(options ...Option) Kindling {
	k := &kindling{
		logWriter: os.Stdout,
	}

	// Sort the options by priority in case some options depend on others.
	sort.Sort(byPriority(options))

	// Apply all the functional options to configure the client.
	for _, opt := range options {
		opt.apply(k)
	}
	return k
}

// NewHTTPClient implements the Kindling interface.
func (k *kindling) NewHTTPClient() *http.Client {
	// Create a specialized HTTP transport that concurrently races between fronted and smart dialer.
	// All options are tried in parallel and the first one to succeed is used.
	// If all options fail, the last error is returned.
	return &http.Client{
		Transport: k.newRaceTransport(),
	}
}

// WithDomainFronting is a functional option that sets up domain fronting for kindling using
// the provided fronted.Fronted instance from https://github.com/getlantern/fronted.
func WithDomainFronting(f fronted.Fronted) Option {
	return newOption(func(k *kindling) {
		slog.Info("Setting domain fronting")
		k.httpDialers = append(k.httpDialers, f.NewConnectedRoundTripper)
	})
}

// WithLogWriter is a functional option that sets the log writer for the Kindling.
// By default, the log writer is set to os.Stdout.
// This should be the first option to be applied to the Kindling to ensure that all logs are captured.
func WithLogWriter(w io.Writer) Option {
	return newOptionWithPriority(func(k *kindling) {
		k.logWriter = w
		log = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{}))
	}, priorityLogWriter)
}

// WithProxyless is a functional option that enables proxyless mode for the Kindling such that
// it accesses the control plane directly using a variety of proxyless techniques.
func WithProxyless(domains ...string) Option {
	return newOption(func(k *kindling) {
		slog.Info("Setting proxyless mode")
		smartDialer, err := newSmartHTTPDialer(k.logWriter, domains...)
		if err != nil {
			log.Error("Failed to create smart dialer", "error", err)
			return
		}
		k.httpDialers = append(k.httpDialers, smartDialer)
	})
}

// WithPanicListener is a functional option that sets a panic listener that should be notified
// whenever any goroutine panics. We set this with a higher priority so that it is set before
// any other options that may depend on it.
func WithPanicListener(panicListener func(string)) Option {
	return newOptionWithPriority(func(k *kindling) {
		slog.Info("Setting panic listener")
		k.panicListener = panicListener
	}, priorityPanicListener) // Set the priority to 0 so that it is set before any other options.
}

func (k *kindling) newRaceTransport() http.RoundTripper {
	// Now create a RoundTripper that races between the available options.
	return newRaceTransport(k.panicListener, k.httpDialers...)
}

func newSmartHTTPDialer(logWriter io.Writer, domains ...string) (httpDialer, error) {
	d, err := newSmartDialer(logWriter, domains...)
	if err != nil {
		return nil, fmt.Errorf("failed to create smart dialer: %v", err)
	}
	return func(ctx context.Context, addr string) (http.RoundTripper, error) {
		streamConn, err := d.DialStream(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial stream: %v", err)
		}
		return newTransportWithDialContext(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return streamConn, nil
		}), nil
	}, nil
}

// NewSmartHTTPTransport creates a new HTTP transport that uses the Outline smart dialer to dial to the
// specified domains.
func NewSmartHTTPTransport(logWriter io.Writer, domains ...string) (*http.Transport, error) {
	d, err := newSmartDialer(logWriter, domains...)
	if err != nil {
		log.Error("Failed to create smart dialer", "error", err)
		return nil, fmt.Errorf("failed to create smart dialer: %v", err)
	}
	return newTransportWithDialContext(func(ctx context.Context, network, addr string) (net.Conn, error) {
		streamConn, err := d.DialStream(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial stream: %v", err)
		}
		return streamConn, nil
	}), nil
}

func newTransportWithDialContext(dialContext func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: 4 * time.Second,
	}
}

//go:embed smart_dialer_config.yml
var embedFS embed.FS

func newSmartDialer(logWriter io.Writer, domains ...string) (transport.StreamDialer, error) {
	finder := &smart.StrategyFinder{
		TestTimeout:  5 * time.Second,
		LogWriter:    logWriter,
		StreamDialer: &transport.TCPDialer{},
		PacketDialer: &transport.UDPDialer{},
	}

	configBytes, err := embedFS.ReadFile("smart_dialer_config.yml")
	if err != nil {
		log.Error("Failed to read smart dialer config", "error", err)
		return nil, err
	}
	dialer, err := finder.NewDialer(context.Background(), domains, configBytes)
	if err != nil {
		log.Error("Failed to create smart dialer", "error", err)
		return nil, err
	}
	return dialer, nil
}

type option struct {
	priorityInt int
	applyFunc   func(*kindling)
}

func (o *option) apply(k *kindling) {
	o.applyFunc(k)
}

func (o *option) priority() int {
	return o.priorityInt
}

// Sometimes we need an option to be set prior to other options that depend on it, so we
// set the priority and store the options prior to exexuting them.
func newOptionWithPriority(apply func(*kindling), priority int) Option {
	return &option{applyFunc: apply, priorityInt: priority}
}

func newOption(apply func(*kindling)) Option {
	return &option{applyFunc: apply, priorityInt: 1000}
}

// byPriority is a type that implements the sort interface for options so that
// we can apply some options before others that may depend on them.
type byPriority []Option

func (bp byPriority) Len() int           { return len(bp) }
func (bp byPriority) Less(i, j int) bool { return bp[i].priority() < bp[j].priority() }
func (bp byPriority) Swap(i, j int)      { bp[i], bp[j] = bp[j], bp[i] }
