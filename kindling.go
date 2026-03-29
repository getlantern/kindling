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
	"sync"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/x/smart"
	"github.com/getlantern/amp"
	"github.com/getlantern/dnstt"
	"github.com/getlantern/fronted"
)

// Kindling creates HTTP clients that race requests across multiple censorship
// circumvention transports, returning the first successful response.
type Kindling interface {
	// NewHTTPClient returns an HTTP client whose transport races all configured
	// circumvention transports in parallel.
	NewHTTPClient() *http.Client

	// ReplaceTransport swaps the round-tripper generator for the named transport,
	// preserving its MaxLength and IsStreamable properties.
	ReplaceTransport(name string, rt func(ctx context.Context, addr string) (http.RoundTripper, error)) error
}

// Transport defines a censorship circumvention transport that can be used by Kindling.
type Transport interface {
	// NewRoundTripper creates a pre-connected http.RoundTripper. Implementations
	// should complete the connection before returning so that the race transport
	// can try requests serially without paying connection latency.
	NewRoundTripper(ctx context.Context, addr string) (http.RoundTripper, error)

	// MaxLength returns the maximum request body size this transport supports.
	// Zero means no limit.
	MaxLength() int

	// IsStreamable reports whether this transport supports streaming responses
	// (e.g. text/event-stream).
	IsStreamable() bool

	// Name identifies this transport for logging and debugging.
	Name() string
}

// Option configures a Kindling instance. Options are applied in the order
// provided — specify WithLogWriter first to capture logs from subsequent
// transport initialization.
type Option func(*kindling) error

type kindling struct {
	mu            sync.Mutex
	log           *slog.Logger
	logWriter     io.Writer
	transports    []Transport
	panicListener func(string)
	appName       string
}

var _ Kindling = (*kindling)(nil)

// NewKindling creates a Kindling instance with the given application name and
// options. Returns an error if any option fails (e.g. a nil transport argument
// or a failed smart dialer initialization).
func NewKindling(name string, options ...Option) (Kindling, error) {
	k := &kindling{
		appName:   name,
		logWriter: os.Stdout,
		log:       slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{AddSource: true})),
	}
	for _, opt := range options {
		if err := opt(k); err != nil {
			return nil, fmt.Errorf("kindling: %w", err)
		}
	}
	if k.panicListener == nil {
		k.panicListener = func(msg string) { k.log.Error(msg) }
	}
	return k, nil
}

// NewHTTPClient returns an HTTP client that races all configured transports.
// Safe to call concurrently with ReplaceTransport.
func (k *kindling) NewHTTPClient() *http.Client {
	k.mu.Lock()
	snapshot := make([]Transport, len(k.transports))
	copy(snapshot, k.transports)
	k.mu.Unlock()

	return &http.Client{
		Transport: newRaceTransport(k.appName, k.log, k.panicListener, snapshot),
	}
}

// ReplaceTransport swaps the round-tripper generator for the named transport.
func (k *kindling) ReplaceTransport(name string, rt func(ctx context.Context, addr string) (http.RoundTripper, error)) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for i, tr := range k.transports {
		if tr.Name() == name {
			k.transports[i] = &namedTransport{
				name:         name,
				maxLength:    tr.MaxLength(),
				isStreamable: tr.IsStreamable(),
				newRT:        rt,
			}
			return nil
		}
	}
	return fmt.Errorf("transport %q not found", name)
}

// --- Options ---

// WithLogWriter sets the log output destination. By default, logs go to
// os.Stdout. Specify this first to capture initialization logs from other
// options like WithProxyless.
func WithLogWriter(w io.Writer) Option {
	return func(k *kindling) error {
		k.logWriter = w
		k.log = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
			AddSource: true,
			Level:     slog.LevelDebug,
		}))
		return nil
	}
}

// WithPanicListener sets a callback invoked when a transport goroutine panics.
func WithPanicListener(fn func(string)) Option {
	return func(k *kindling) error {
		k.panicListener = fn
		return nil
	}
}

// WithTransport adds a custom Transport implementation.
func WithTransport(t Transport) Option {
	return func(k *kindling) error {
		if t == nil {
			return fmt.Errorf("transport is nil")
		}
		k.transports = append(k.transports, t)
		return nil
	}
}

// WithDomainFronting adds domain fronting via the provided fronted.Fronted.
func WithDomainFronting(f fronted.Fronted) Option {
	return func(k *kindling) error {
		if f == nil {
			return fmt.Errorf("fronted instance is nil")
		}
		k.transports = append(k.transports, &namedTransport{
			name:         "fronted",
			isStreamable: true,
			newRT:        f.NewConnectedRoundTripper,
		})
		return nil
	}
}

// WithDNSTunnel adds DNS tunneling via the provided dnstt.DNSTT.
func WithDNSTunnel(d dnstt.DNSTT) Option {
	return func(k *kindling) error {
		if d == nil {
			return fmt.Errorf("dnstt instance is nil")
		}
		k.transports = append(k.transports, &namedTransport{
			name:         "dnstt",
			isStreamable: true,
			newRT:        d.NewRoundTripper,
		})
		return nil
	}
}

// WithAMPCache adds AMP caching via the provided amp.Client.
// AMP has a 6000-byte request body limit and does not support streaming.
func WithAMPCache(c amp.Client) Option {
	return func(k *kindling) error {
		if c == nil {
			return fmt.Errorf("amp client is nil")
		}
		k.transports = append(k.transports, &namedTransport{
			name:      "amp",
			maxLength: 6000,
			newRT: func(ctx context.Context, addr string) (http.RoundTripper, error) {
				return c.RoundTripper()
			},
		})
		return nil
	}
}

// WithProxyless enables direct access using the Outline SDK smart dialer,
// which bypasses DNS-based and SNI-based blocking.
func WithProxyless(domains ...string) Option {
	return func(k *kindling) error {
		dialer, err := newSmartDialer(k.logWriter, domains...)
		if err != nil {
			return fmt.Errorf("creating smart dialer: %w", err)
		}
		k.transports = append(k.transports, &namedTransport{
			name:         "smart",
			isStreamable: true,
			newRT: func(ctx context.Context, addr string) (http.RoundTripper, error) {
				conn, err := dialer.DialStream(ctx, addr)
				if err != nil {
					return nil, fmt.Errorf("smart dial: %w", err)
				}
				return preconnectedTransport(conn), nil
			},
		})
		return nil
	}
}

// --- Internal transport type ---

type namedTransport struct {
	name         string
	maxLength    int
	isStreamable bool
	newRT        func(ctx context.Context, addr string) (http.RoundTripper, error)
}

func (t *namedTransport) Name() string       { return t.name }
func (t *namedTransport) MaxLength() int     { return t.maxLength }
func (t *namedTransport) IsStreamable() bool { return t.isStreamable }

func (t *namedTransport) NewRoundTripper(ctx context.Context, addr string) (http.RoundTripper, error) {
	return t.newRT(ctx, addr)
}

// --- Smart dialer ---

//go:embed smart_dialer_config.yml
var configFS embed.FS

func newSmartDialer(logWriter io.Writer, domains ...string) (transport.StreamDialer, error) {
	configBytes, err := configFS.ReadFile("smart_dialer_config.yml")
	if err != nil {
		return nil, fmt.Errorf("reading smart dialer config: %w", err)
	}
	finder := &smart.StrategyFinder{
		TestTimeout:  5 * time.Second,
		LogWriter:    logWriter,
		StreamDialer: &transport.TCPDialer{},
		PacketDialer: &transport.UDPDialer{},
	}
	return finder.NewDialer(context.Background(), domains, configBytes)
}

// preconnectedTransport creates an http.Transport that uses an already-established
// connection. Intended for single-request use within the race transport.
func preconnectedTransport(conn net.Conn) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return conn, nil
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:  20 * time.Second,
		ExpectContinueTimeout: 4 * time.Second,
	}
}

// NewSmartHTTPTransport creates an http.Transport using the Outline SDK smart
// dialer for the given domains. This is a standalone utility that does not
// require a Kindling instance.
func NewSmartHTTPTransport(logWriter io.Writer, domains ...string) (*http.Transport, error) {
	dialer, err := newSmartDialer(logWriter, domains...)
	if err != nil {
		return nil, err
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialStream(ctx, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:  20 * time.Second,
		ExpectContinueTimeout: 4 * time.Second,
	}, nil
}
