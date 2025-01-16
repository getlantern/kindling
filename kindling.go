package kindling

import (
	"context"
	"embed"
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

type kindling struct {
	roundTrippers []http.RoundTripper
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
		k.roundTrippers = append(k.roundTrippers, newFronted(configURL, countryCode))
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
		k.roundTrippers = append(k.roundTrippers, newSmartRoundTripper(domains...))
	}
}

func (k *kindling) newRaceTransport() http.RoundTripper {
	// Now create a RoundTripper that races between the available options.
	return newRaceTransport(k.roundTrippers...)
}

func newFronted(configURL, countryCode string) fronted.Fronted {
	// Parse the domain from the URL.
	u, err := url.Parse(configURL)
	if err != nil {
		slog.Error("Failed to parse URL", "error", err)
		return nil
	}
	// Extract the domain from the URL.
	domain := u.Host

	// First, download the file from the specified URL using the smart dialer.
	// Then, create a new fronted instance with the downloaded file.
	httpClient := &http.Client{
		Transport: newSmartRoundTripper(domain),
	}

	f := fronted.NewFronted(
		fronted.WithHTTPClient(httpClient),
		fronted.WithConfigURL(configURL),
		fronted.WithCountryCode(countryCode),
	)
	return f
}

func newSmartRoundTripper(domains ...string) http.RoundTripper {
	d := newSmartDialer(domains...)
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			streamConn, err := d.DialStream(ctx, addr)
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
}

//go:embed smart_dialer_config.yml
var embedFS embed.FS

func newSmartDialer(domains ...string) transport.StreamDialer {
	finder := &smart.StrategyFinder{
		TestTimeout:  5 * time.Second,
		LogWriter:    os.Stdout,
		StreamDialer: &transport.TCPDialer{},
		PacketDialer: &transport.UDPDialer{},
	}

	configBytes, err := embedFS.ReadFile("smart_dialer_config.yml")
	if err != nil {
		slog.Error("Failed to read smart dialer config", "error", err)
	}
	dialer, err := finder.NewDialer(context.Background(), domains, configBytes)
	if err != nil {
		slog.Error("Failed to create smart dialer", "error", err)
	}
	return dialer
}
