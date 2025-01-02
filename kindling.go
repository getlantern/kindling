package kindling

import (
	"context"
	"net"
)

// Kindling is the interface that wraps the basic Dial and DialContext methods for control
// plane traffic.
type Kindling interface {

	// Dial connects to the address on the named network.
	Dial(network, address string) (net.Conn, error)

	// DialContext connects to the address on the named network using the provided context.
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type kindling struct {
}

// Make sure that kindling implements the Kindling interface.
var _ Kindling = &kindling{}

// Option is a functional option type that allows us to configure the Client.
type Option func(Kindling)

// NewKindling returns a new Kindling.
func NewKindling(options ...Option) Kindling {
	m := &kindling{}
	// Apply all the functional options to configure the client.
	for _, opt := range options {
		opt(m)
	}

	return m
}

// Dial implements the Kindling interface.
func (m *kindling) Dial(network, address string) (net.Conn, error) {
	return m.DialContext(context.Background(), network, address)
}

// DialContext implements the Kindling interface.
func (m *kindling) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return nil, nil
}

// WithDomainFronting is a functional option that enables domain fronting for the Kindling.
func WithDomainFronting() Option {
	return func(m Kindling) {

	}
}

// WithDoHTunnel is a functional option that enables DNS over HTTPS (DoH) tunneling for the Kindling.
func WithDoHTunnel() Option {
	return func(m Kindling) {

	}
}

// WithProxyless is a functional option that enables proxyless mode for the Kindling such that
// it accesses the control plane directly using a variety of proxyless techniques.
func WithProxyless(domain string) Option {
	return func(m Kindling) {

	}
}
