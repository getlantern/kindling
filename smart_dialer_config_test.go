package kindling

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/x/configurl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const strategyTestServerName = "smart-dialer-config-test"

func embeddedStrategies(t *testing.T) []string {
	t.Helper()
	raw, err := configFS.ReadFile("smart_dialer_config.yml")
	require.NoError(t, err)

	var cfg struct {
		TLS []string `yaml:"tls"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	// An empty list would make the caller's loop pass vacuously.
	require.NotEmpty(t, cfg.TLS, "the embedded config lists no tls strategy")
	return cfg.TLS
}

// newStrategyTestServer returns a TLS listener's address and a pool trusting its
// self-signed certificate. Strategies rewrite the stream around TLS records and
// write boundaries, so exercising them needs a real ClientHello.
func newStrategyTestServer(t *testing.T) (string, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: strategyTestServerName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{strategyTestServerName},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(cert)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.(*tls.Conn).HandshakeContext(context.Background())
			}()
		}
	}()
	return ln.Addr().String(), roots
}

// TestEmbeddedStrategiesDial builds every strategy in the embedded config and
// completes a TLS handshake through it.
//
// A strategy that cannot dial at all is invisible in production: the finder
// races the list and discards whatever fails, so a misspelled entry costs a
// circumvention technique while everything still appears to work.
func TestEmbeddedStrategiesDial(t *testing.T) {
	t.Parallel()

	addr, roots := newStrategyTestServer(t)
	for _, strategy := range embeddedStrategies(t) {
		name := strategy
		if name == "" {
			name = "direct"
		}
		t.Run(name, func(t *testing.T) {
			providers := configurl.NewDefaultProviders()
			providers.StreamDialers.BaseInstance = &transport.TCPDialer{}
			dialer, err := providers.NewStreamDialer(context.Background(), strategy)
			require.NoError(t, err, "%q is not a valid outline-sdk config", strategy)

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			conn, err := dialer.DialStream(ctx, addr)
			if !assert.NoError(t, err, "%q cannot dial", strategy) {
				return
			}
			defer conn.Close()

			tlsConn := tls.Client(conn, &tls.Config{
				ServerName: strategyTestServerName,
				RootCAs:    roots,
			})
			defer tlsConn.Close()
			assert.NoError(t, tlsConn.HandshakeContext(ctx), "%q cannot complete a handshake", strategy)
		})
	}
}
