package kindling

import (
	"context"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/x/smart"
	"github.com/getlantern/fronted"
)

// Kindling is the interface that wraps the basic Dial and DialContext methods for control
// plane traffic.
type Kindling interface {

	// NewHTTPClient returns a new HTTP client that is configured to use kindling.
	NewHTTPClient() *http.Client
}

type kindling struct {
	fronted     fronted.Fronted
	smartDialer transport.StreamDialer
}

// Make sure that kindling implements the Kindling interface.
var _ Kindling = &kindling{}

// Option is a functional option type that allows us to configure the Client.
type Option func(*kindling)

// NewKindling returns a new Kindling.
func NewKindling(options ...Option) Kindling {
	m := &kindling{}
	// Apply all the functional options to configure the client.
	for _, opt := range options {
		opt(m)
	}

	return m
}

// NewHTTPClient implements the Kindling interface.
func (m *kindling) NewHTTPClient() *http.Client {
	// Create a specialized HTTP transport that concurrently races between fronted and smart dialer.
	// All options are tried in parallel and the first one to succeed is used.
	// If all options fail, the last error is returned.
	return &http.Client{
		Transport: newRaceTransport(m.fronted, m.smartDialer),
	}
}

func newRaceTransport(fronted fronted.Fronted, smartDialer transport.StreamDialer) http.RoundTripper {
	// First, create a RoundTripper from the smart dialer.
	smartTransport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			streamConn, err := smartDialer.DialStream(ctx, addr)
			if err != nil {
				return nil, err
			}
			return streamConn, nil
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	// Now create a RoundTripper that races between the fronted and smart dialer.
	return &raceTransport{
		fronted:        fronted,
		smartTransport: smartTransport,
	}
}

// WithDomainFronting is a functional option that enables domain fronting for the Kindling.
func WithDomainFronting(pool *x509.CertPool, providers map[string]*fronted.Provider) Option {
	return func(m *kindling) {
		m.fronted = newFronted(pool, providers)
	}
}

// WithDoHTunnel is a functional option that enables DNS over HTTPS (DoH) tunneling for the Kindling.
func WithDoHTunnel() Option {
	return func(m *kindling) {

	}
}

// WithProxyless is a functional option that enables proxyless mode for the Kindling such that
// it accesses the control plane directly using a variety of proxyless techniques.
func WithProxyless(domains ...string) Option {
	return func(m *kindling) {
		m.smartDialer = newSmartDialer(domains...)
	}
}

func newFronted(pool *x509.CertPool, providers map[string]*fronted.Provider) fronted.Fronted {
	var cacheFile string
	dir, err := os.UserConfigDir()
	if err != nil {
		slog.Error("Unable to get user config dir: %w", err)
	} else {
		cacheFile = filepath.Join(dir, "fronted", "fronted_cache.json")
	}
	f := fronted.NewFronted(cacheFile)
	f.OnNewFronts(pool, providers)
	return f
}

func newSmartDialer(domains ...string) transport.StreamDialer {
	finder := &smart.StrategyFinder{
		TestTimeout:  5 * time.Second,
		LogWriter:    os.Stdout,
		StreamDialer: &transport.TCPDialer{},
		PacketDialer: &transport.UDPDialer{},
	}

	configBytes := []byte(`
	{
	  "dns": [
		  {"system": {}},
		  {"https": {"name": "8.8.8.8"}},
		  {"https": {"name": "9.9.9.9"}}
	  ],
	  "tls": [
		  "",
		  "split:2",
		  "tlsfrag:1"
	  ]
	}
	`)

	dialer, err := finder.NewDialer(context.Background(), domains, configBytes)
	if err != nil {
		slog.Error("Failed to create smart dialer", "error", err)
	}
	return dialer
}
