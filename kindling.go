package kindling

import (
	"context"
	"crypto/x509"
	"embed"
	"encoding/pem"
	"fmt"
	"io"
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

var log *slog.Logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{}))

// Kindling is the interface that wraps the basic Dial and DialContext methods for control
// plane traffic.
type Kindling interface {
	// NewHTTPClient returns a new HTTP client that is configured to use kindling.
	NewHTTPClient() *http.Client
}

type httpDialer func(ctx context.Context, addr string) (http.RoundTripper, error)

type kindling struct {
	httpDialers   []httpDialer
	rootCA        string
	logWriter     io.Writer
	panicListener func(string)
}

// Make sure that kindling implements the Kindling interface.
var _ Kindling = &kindling{}

// Option is a functional option type that allows us to configure the Client.
type Option func(*kindling)

// NewKindling returns a new Kindling.
func NewKindling(options ...Option) Kindling {
	k := &kindling{
		logWriter: os.Stdout,
	}

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
		frontedDialer, err := k.newFrontedDialer(configURL, countryCode)
		if err != nil {
			log.Error("Failed to create fronted dialer", "error", err)
			return
		}
		k.httpDialers = append(k.httpDialers, frontedDialer)
	}
}

// WithRootCA pins the root CA to use for TLS.
func WithRootCA(rootCA string) Option {
	return func(k *kindling) {
		k.rootCA = rootCA
	}
}

// WithLogWriter is a functional option that sets the log writer for the Kindling.
// By default, the log writer is set to os.Stdout.
// This should be the first option to be applied to the Kindling to ensure that all logs are captured.
func WithLogWriter(w io.Writer) Option {
	return func(k *kindling) {
		k.logWriter = w
		log = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{}))
	}
}

// WithProxyless is a functional option that enables proxyless mode for the Kindling such that
// it accesses the control plane directly using a variety of proxyless techniques.
func WithProxyless(domains ...string) Option {
	return func(k *kindling) {
		smartDialer, err := k.newSmartHTTPDialer(domains...)
		if err != nil {
			log.Error("Failed to create smart dialer", "error", err)
			return
		}
		k.httpDialers = append(k.httpDialers, smartDialer)
	}
}

func WithPanicListener(panicListener func(string)) Option {
	return func(k *kindling) {
		k.panicListener = panicListener
	}
}

func (k *kindling) newRaceTransport() http.RoundTripper {
	// Now create a RoundTripper that races between the available options.
	return newRaceTransport(k.panicListener, k.httpDialers...)
}

func (k *kindling) newFrontedDialer(configURL, countryCode string) (httpDialer, error) {
	// Parse the domain from the URL.
	u, err := url.Parse(configURL)
	if err != nil {
		log.Error("Failed to parse URL", "error", err)
		return nil, fmt.Errorf("failed to parse URL: %v", err)
	}
	// Extract the domain from the URL.
	domain := u.Host

	// First, download the file from the specified URL using the smart dialer.
	// Then, create a new fronted instance with the downloaded file.
	trans, err := k.newSmartHTTPTransport(domain)
	if err != nil {
		log.Error("Failed to create smart HTTP transport", "error", err)
		return nil, fmt.Errorf("failed to create smart HTTP transport: %v", err)
	}
	httpClient := &http.Client{
		Transport: trans,
	}
	fr := fronted.NewFronted(
		fronted.WithPanicListener(k.panicListener),
		fronted.WithHTTPClient(httpClient),
		fronted.WithConfigURL(configURL),
		fronted.WithCountryCode(countryCode),
	)
	return fr.NewConnectedRoundTripper, nil
}

func (k *kindling) newSmartHTTPDialer(domains ...string) (httpDialer, error) {
	d, err := k.newSmartDialer(domains...)
	if err != nil {
		return nil, fmt.Errorf("failed to create smart dialer: %v", err)
	}
	return func(ctx context.Context, addr string) (http.RoundTripper, error) {
		streamConn, err := d.DialStream(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial stream: %v", err)
		}
		return k.newTransportWithDialContext(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return streamConn, nil
		})
	}, nil
}

func (k *kindling) newSmartHTTPTransport(domains ...string) (*http.Transport, error) {
	d, err := k.newSmartDialer(domains...)
	if err != nil {
		log.Error("Failed to create smart dialer", "error", err)
		return nil, fmt.Errorf("failed to create smart dialer: %v", err)
	}
	return k.newTransportWithDialContext(func(ctx context.Context, network, addr string) (net.Conn, error) {
		streamConn, err := d.DialStream(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("failed to dial stream: %v", err)
		}
		return streamConn, nil
	})
}

func (k *kindling) newTransportWithDialContext(dialContext func(ctx context.Context, network, addr string) (net.Conn, error)) (*http.Transport, error) {
	tr := &http.Transport{
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: 4 * time.Second,
	}
	if k.rootCA != "" {
		block, _ := pem.Decode([]byte(k.rootCA))
		if block == nil {
			return nil, fmt.Errorf("failed to decode root CA PEM block")
		}
		certPool := x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(block.Bytes) {
			log.Error("Failed to append root CA to pool")
			return nil, fmt.Errorf("failed to append root CA to pool")
		}
		tr.TLSClientConfig.RootCAs = certPool
	}
	return tr, nil
}

//go:embed smart_dialer_config.yml
var embedFS embed.FS

func (k *kindling) newSmartDialer(domains ...string) (transport.StreamDialer, error) {
	finder := &smart.StrategyFinder{
		TestTimeout:  5 * time.Second,
		LogWriter:    k.logWriter,
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
