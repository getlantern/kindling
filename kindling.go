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
	"sync"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/x/smart"
	"github.com/getlantern/amp"
	"github.com/getlantern/dnstt"
	"github.com/getlantern/fronted"
)

var log = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{AddSource: true}))

// Kindling is the interface that wraps the basic Dial and DialContext methods for control
// plane traffic.
type Kindling interface {
	// NewHTTPClient returns a new HTTP client that is configured to use kindling.
	NewHTTPClient() *http.Client
	// ReplaceTransport replaces an existing transport RoundTripper generator with the provided one.
	ReplaceTransport(name string, rt func(ctx context.Context, addr string) (http.RoundTripper, error)) error
}
type roundTripperGenerator func(ctx context.Context, addr string) (http.RoundTripper, error)

//type httpDialer func(ctx context.Context, addr string) (http.RoundTripper, error)

type kindling struct {
	roundTripperGeneratorsMutex sync.Mutex
	transports                  []Transport
	logWriter                   io.Writer
	panicListener               func(string)
	appName                     string // The name of the tool using kindling, used for logging and debugging.
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

// NewKindling returns a new Kindling with the specified name of your tool and the options to use for
// accessing control plane data.
func NewKindling(name string, options ...Option) Kindling {
	k := &kindling{
		logWriter: os.Stdout,
		appName:   name,
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

func (k *kindling) ReplaceTransport(name string, rt func(ctx context.Context, addr string) (http.RoundTripper, error)) error {
	k.roundTripperGeneratorsMutex.Lock()
	defer k.roundTripperGeneratorsMutex.Unlock()

	for i, tr := range k.transports {
		slog.Info("Checking transport", "name", tr.Name())
		if tr.Name() == name {
			k.transports[i] = newTransport(name, tr.MaxLength(), rt)
			return nil
		}
	}
	return fmt.Errorf("Could not find matching transport: %v", name)
}

// WithDomainFronting is a functional option that sets up domain fronting for kindling using
// the provided fronted.Fronted instance from https://github.com/getlantern/fronted.
func WithDomainFronting(f fronted.Fronted) Option {
	log.Info("Setting domain fronting")
	if f == nil {
		log.Error("Fronted instance is nil")
		return &emptyOption{}
	}
	return WithTransport(newTransport("fronted", 0, func(ctx context.Context, addr string) (http.RoundTripper, error) {
		return f.NewConnectedRoundTripper(ctx, addr)
	}))
}

// WithDNSTunnel is a functional option that sets up a DNS tunnel for kindling using the provided
// [dnstt.DNSTT] instance
func WithDNSTunnel(d dnstt.DNSTT) Option {
	log.Info("Setting DNS tunnel")
	if d == nil {
		log.Error("DNSTT instance is nil")
		return &emptyOption{}
	}
	return WithTransport(newTransport("dnstt", 0, func(ctx context.Context, addr string) (http.RoundTripper, error) {
		return d.NewRoundTripper(ctx, addr)
	}))
}

// WithAMPCache uses the AMP cache for making requests. It adds an 'amp' round tripper from the provided amp.Client.
func WithAMPCache(c amp.Client) Option {
	log.Info("Setting AMP cache")
	if c == nil {
		log.Error("AMP client is nil")
		return &emptyOption{}
	}
	return WithTransport(newTransport("amp", 6000, func(ctx context.Context, addr string) (http.RoundTripper, error) {
		return c.RoundTripper()
	}))
}

// WithProxyless is a functional option that enables proxyless mode for the Kindling such that
// it accesses the control plane directly using a variety of proxyless techniques.
func WithProxyless(domains ...string) Option {
	return newOption(func(k *kindling) {
		slog.Info("Setting proxyless mode")
		smartDialer, err := newSmartHTTPDialerFunc(k.logWriter, domains...)
		if err != nil {
			log.Error("Failed to create smart dialer", "error", err)
			return
		}
		k.transports = append(k.transports, newTransport("smart", 0, smartDialer))
	})
}

// WithLogWriter is a functional option that sets the log writer for the Kindling.
// By default, the log writer is set to os.Stdout.
// This should be the first option to be applied to the Kindling to ensure that all logs are captured.
func WithLogWriter(w io.Writer) Option {
	return newOptionWithPriority(func(k *kindling) {
		k.logWriter = w
		log = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
			AddSource: true,
			Level:     slog.LevelDebug, // Set the log level to debug for detailed output
		}))
	}, priorityLogWriter)
}

// Transport provides the basic interface that any transport must implement to be used by Kindling.
type Transport interface {
	// NewRoundTripper creates a new http.RoundTripper that uses this transport. As much as possible
	// the RoundTripper should be pre-connected when it is returned, as otherwise it can take too
	// much time away from other transports. In other words, Kindling parallelizes the connection
	// of the transports, but the actual sending of the request is done serially to avoid
	// issues with non-idempotent requests.
	NewRoundTripper(ctx context.Context, addr string) (http.RoundTripper, error)

	// MaxLength returns the maximum length of data that can be sent using this transport, if any.
	// A value of 0 means there is no limit.
	MaxLength() int

	// Name returns the name of the transport for logging and debugging purposes.
	Name() string
}

// WithTransport allows users to add any transport matching the minimal Transport interface.
func WithTransport(transport Transport) Option {
	return newOption(func(k *kindling) {
		log.Info("Setting custom transport")
		if transport == nil {
			log.Error("Transport instance is nil")
			return
		}
		k.transports = append(k.transports, transport)
	})
}

// WithPanicListener is a functional option that sets a panic listener that should be notified
// whenever any goroutine panics. We set this with a higher priority so that it is set before
// any other options that may depend on it.
func WithPanicListener(panicListener func(string)) Option {
	return newOptionWithPriority(func(k *kindling) {
		log.Info("Setting panic listener")
		k.panicListener = panicListener
	}, priorityPanicListener) // Set the priority to 0 so that it is set before any other options.
}

func (k *kindling) newRaceTransport() http.RoundTripper {
	// Now create a RoundTripper that races between the available options.
	return newRaceTransport(k.appName, k.panicListener, k.transports...)
}

func newSmartHTTPDialerFunc(logWriter io.Writer, domains ...string) (roundTripperGenerator, error) {
	d, err := newSmartDialer(logWriter, domains...)
	if err != nil {
		return nil, fmt.Errorf("failed to create smart dialer: %v", err)
	}
	return func(ctx context.Context, addr string) (http.RoundTripper, error) {
		streamConn, err := d.DialStream(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial stream in smart dialer: %v", err)
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
// set the priority and store the options prior to executing them.
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

type emptyOption struct{}

func (o *emptyOption) apply(k *kindling) {}
func (o *emptyOption) priority() int     { return 0 }

type namedTransport struct {
	name      string
	maxLength int
	rtg       roundTripperGenerator
}

func (t *namedTransport) NewRoundTripper(ctx context.Context, addr string) (http.RoundTripper, error) {
	return t.rtg(ctx, addr)
}

func (t *namedTransport) MaxLength() int {
	return t.maxLength
}

func (t *namedTransport) Name() string {
	return t.name
}

func newTransport(name string, maxLength int, rtg roundTripperGenerator) Transport {
	return &namedTransport{
		name:      name,
		maxLength: maxLength,
		rtg:       rtg,
	}
}
