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

// NewMaestro returns a new Maestro.
func NewMaestro() Maestro {
	return &maestro{}
}

// Dial implements the Maestro interface.
func (m *maestro) Dial(network, address string) (net.Conn, error) {
	return nil, nil
}

// DialContext implements the Maestro interface.
func (m *maestro) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return nil, nil
}
