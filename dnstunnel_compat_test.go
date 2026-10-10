package kindling

import (
	"testing"

	"github.com/getlantern/dnstt"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/kindling/dnstunnel"
)

// The spark-protocol DNS tunnel plugs into the existing WithDNSTunnel slot.
var _ dnstt.DNSTT = (*dnstunnel.Client)(nil)

func TestWithDNSTunnelAcceptsSparkProtocolClient(t *testing.T) {
	c, err := dnstunnel.New(dnstunnel.Config{
		Zone:            "t.example.com",
		ServerPublicKey: "Ty6wBHf3XGrUuC4+Q6mJd2TbeKpW4b4l2cVLmw9o4Yk=",
		Resolvers:       []string{"192.0.2.1"},
	})
	require.NoError(t, err)
	defer c.Close()
	_, err = NewKindling("test", WithDNSTunnel(c))
	require.NoError(t, err)
}
