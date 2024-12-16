package maestro

import (
	"context"
	"net"
)

// Maestro is the interface that wraps the basic Dial and DialContext methods for control
// plane traffic.
type Maestro interface {

	// Dial connects to the address on the named network.
	Dial(network, address string) (net.Conn, error)

	// DialContext connects to the address on the named network using the provided context.
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type maestro struct {
}

// Make sure that maestro implements the Maestro interface.
var _ Maestro = &maestro{}

// Option is a functional option type that allows us to configure the Client.
type Option func(Maestro)

// NewMaestro returns a new Maestro.
func NewMaestro(options ...Option) Maestro {
	m := &maestro{}
	// Apply all the functional options to configure the client.
	for _, opt := range options {
		opt(m)
	}

	return m
}

// Dial implements the Maestro interface.
func (m *maestro) Dial(network, address string) (net.Conn, error) {
	return nil, nil
}

// DialContext implements the Maestro interface.
func (m *maestro) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return nil, nil
}

// WithDomainFronting is a functional option that enables domain fronting for the Maestro.
func WithDomainFronting() Option {
	return func(m Maestro) {

	}
}

// WithDoHTunnel is a functional option that enables DNS over HTTPS (DoH) tunneling for the Maestro.
func WithDoHTunnel() Option {
	return func(m Maestro) {

	}
}

// WithProxyless is a functional option that enables proxyless mode for the Maestro such that
// it accesses the control plane directly using a variety of proxyless techniques.
func WithProxyless(domain string) Option {
	return func(m Maestro) {

	}
}
