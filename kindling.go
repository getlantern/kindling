package kindling

import (
	"crypto/x509"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/getlantern/fronted"
)

// Kindling is the interface that wraps the basic Dial and DialContext methods for control
// plane traffic.
type Kindling interface {

	// NewHTTPClient returns a new HTTP client that is configured to use kindling.
	NewHTTPClient() *http.Client
}

type kindling struct {
	fronted fronted.Fronted
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
	return &http.Client{
		Transport: m.fronted,
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
func WithProxyless(domain string) Option {
	return func(m *kindling) {

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
