package kindling

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
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

type httpDialer func(ctx context.Context, addr string) (http.RoundTripper, error)

type kindling struct {
	httpDialers []httpDialer
}

// Make sure that kindling implements the Kindling interface.
var _ Kindling = &kindling{}

// Option is a functional option type that allows us to configure the Client.
type Option func(*kindling)

// NewKindling returns a new Kindling.
func NewKindling(options ...Option) Kindling {
	k := &kindling{}
	// Apply all the functional options to configure the client.
	for _, opt := range options {
		opt(k)
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

// WithDomainFronting is a functional option that enables domain fronting for the Kindling.
func WithDomainFronting(configURL, countryCode string) Option {
	return func(k *kindling) {
		frontedDialer, err := newFrontedDialer(configURL, countryCode)
		if err != nil {
			slog.Error("Failed to create fronted dialer", "error", err)
			return
		}
		k.httpDialers = append(k.httpDialers, frontedDialer)
	}
}

// WithDoHTunnel is a functional option that enables DNS over HTTPS (DoH) tunneling for the Kindling.
func WithDoHTunnel() Option {
	return func(k *kindling) {

	}
}

// WithProxyless is a functional option that enables proxyless mode for the Kindling such that
// it accesses the control plane directly using a variety of proxyless techniques.
func WithProxyless(domains ...string) Option {
	return func(k *kindling) {
		k.httpDialers = append(k.httpDialers, newSmartHTTPDialer(domains...))
	}
}

func (k *kindling) newRaceTransport() http.RoundTripper {
	// Now create a RoundTripper that races between the available options.
	return newRaceTransport(k.httpDialers...)
}

func newFrontedDialer(configURL, countryCode string) (httpDialer, error) {
	// Parse the domain from the URL.
	u, err := url.Parse(configURL)
	if err != nil {
		slog.Error("Failed to parse URL", "error", err)
		return nil, fmt.Errorf("failed to parse URL: %v", err)
	}
	// Extract the domain from the URL.
	domain := u.Host

	// First, download the file from the specified URL using the smart dialer.
	// Then, create a new fronted instance with the downloaded file.
	transport, err := newSmartHTTPTransport(domain)
	if err != nil {
		slog.Error("Failed to create smart HTTP transport", "error", err)
		return nil, fmt.Errorf("failed to create smart HTTP transport: %v", err)
	}
	httpClient := &http.Client{
		Transport: transport,
	}
	fr := fronted.NewFronted(
		fronted.WithHTTPClient(httpClient),
		fronted.WithConfigURL(configURL),
		fronted.WithCountryCode(countryCode),
	)
	return fr.NewConnectedRoundTripper, nil
}

func newSmartHTTPDialer(domains ...string) httpDialer {
	return func(ctx context.Context, addr string) (http.RoundTripper, error) {
		d, err := newSmartDialer(domains...)
		if err != nil {
			return nil, fmt.Errorf("failed to create smart dialer: %v", err)
		}
		streamConn, err := d.DialStream(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial stream: %v", err)
		}
		return newTransportWithDialContect(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return streamConn, nil
		}), nil
	}
}

func newSmartHTTPTransport(domains ...string) (*http.Transport, error) {
	d, err := newSmartDialer(domains...)
	if err != nil {
		slog.Error("Failed to create smart dialer", "error", err)
		return nil, fmt.Errorf("failed to create smart dialer: %v", err)
	}
	return newTransportWithDialContect(func(ctx context.Context, network, addr string) (net.Conn, error) {
		streamConn, err := d.DialStream(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial stream: %v", err)
		}
		return streamConn, nil
	}), nil
}
func newTransportWithDialContect(dialContext func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
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

func newSmartDialer(domains ...string) (transport.StreamDialer, error) {
	finder := &smart.StrategyFinder{
		TestTimeout:  5 * time.Second,
		LogWriter:    os.Stdout,
		StreamDialer: &transport.TCPDialer{},
		PacketDialer: &transport.UDPDialer{},
	}

	configBytes, err := embedFS.ReadFile("smart_dialer_config.yml")
	if err != nil {
		slog.Error("Failed to read smart dialer config", "error", err)
		return nil, err
	}
	dialer, err := finder.NewDialer(context.Background(), domains, configBytes)
	if err != nil {
		slog.Error("Failed to create smart dialer", "error", err)
		return nil, err
	}
	return dialer, nil
}
