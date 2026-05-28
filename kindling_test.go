package kindling

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/domainfront"
)

func TestNewKindling(t *testing.T) {
	t.Parallel()

	t.Run("Success", func(t *testing.T) {
		t.Parallel()

		cfg := &domainfront.Config{
			Providers: map[string]*domainfront.Provider{
				"cloudfront": {
					HostAliases: map[string]string{"example.com": "d1234.cloudfront.net"},
					TestURL:     "https://d1234.cloudfront.net/ping",
					Masquerades: []*domainfront.Masquerade{
						{Domain: "d5678.cloudfront.net", IpAddress: "13.224.0.1"},
					},
				},
				"akamai": {
					HostAliases: map[string]string{"api.example.com": "api.dsa.akamai.example.com"},
					TestURL:     "https://fronted-ping.dsa.akamai.example.com/ping",
					Masquerades: []*domainfront.Masquerade{
						{Domain: "a248.e.akamai.net", IpAddress: "23.192.228.145"},
					},
				},
			},
		}
		c, err := domainfront.New(context.Background(), cfg)
		if err != nil {
			t.Fatalf("domainfront.New() error = %v", err)
		}
		defer c.Close()

		k, err := NewKindling("kindling",
			WithDomainFronting(c),
			WithPanicListener(func(string) {}),
		)
		if err != nil {
			t.Fatalf("NewKindling() error = %v", err)
		}
		if k == nil {
			t.Error("NewKindling() = nil; want non-nil")
		}
	})

	t.Run("NilFronted_ReturnsError", func(t *testing.T) {
		t.Parallel()
		_, err := NewKindling("test", WithDomainFronting(nil))
		if err == nil {
			t.Error("NewKindling(WithDomainFronting(nil)) should return error")
		}
	})

	t.Run("NilDNSTT_ReturnsError", func(t *testing.T) {
		t.Parallel()
		_, err := NewKindling("test", WithDNSTunnel(nil))
		if err == nil {
			t.Error("NewKindling(WithDNSTunnel(nil)) should return error")
		}
	})

	t.Run("NilAMP_ReturnsError", func(t *testing.T) {
		t.Parallel()
		_, err := NewKindling("test", WithAMPCache(nil))
		if err == nil {
			t.Error("NewKindling(WithAMPCache(nil)) should return error")
		}
	})

	t.Run("NilTransport_ReturnsError", func(t *testing.T) {
		t.Parallel()
		_, err := NewKindling("test", WithTransport(nil))
		if err == nil {
			t.Error("NewKindling(WithTransport(nil)) should return error")
		}
	})

	// WithStreamDialer / WithPacketDialer must take effect on WithProxyless
	// regardless of the order callers pass them. The smart dialer is
	// constructed in a deferred pass after every option has had a chance to
	// set streamDialer/packetDialer on the struct — this test guards that
	// pass from regressing back to in-option construction.
	t.Run("ProxylessDialerOrderIndependent", func(t *testing.T) {
		stream := stubStreamDialer{}
		packet := stubPacketDialer{}
		cases := []struct {
			name string
			opts []Option
		}{
			{"dialer-before-proxyless", []Option{
				WithStreamDialer(stream),
				WithPacketDialer(packet),
				WithProxyless("example.com"),
			}},
			{"proxyless-before-dialer", []Option{
				WithProxyless("example.com"),
				WithStreamDialer(stream),
				WithPacketDialer(packet),
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var gotStream transport.StreamDialer
				var gotPacket transport.PacketDialer
				orig := newSmartDialerFn
				newSmartDialerFn = func(_ io.Writer, _ []byte, s transport.StreamDialer, p transport.PacketDialer, _ ...string) (transport.StreamDialer, error) {
					gotStream, gotPacket = s, p
					return stubStreamDialer{}, nil
				}
				t.Cleanup(func() { newSmartDialerFn = orig })

				k, err := NewKindling("test", tc.opts...)
				if err != nil {
					t.Fatalf("NewKindling() error = %v", err)
				}
				if gotStream != stream {
					t.Errorf("newSmartDialer stream arg = %v; want %v", gotStream, stream)
				}
				if gotPacket != packet {
					t.Errorf("newSmartDialer packet arg = %v; want %v", gotPacket, packet)
				}
				ki := k.(*kindling)
				if len(ki.transports) != 1 || ki.transports[0].Name() != string(TransportSmart) {
					t.Errorf("transports = %v; want one %q", ki.transports, TransportSmart)
				}
			})
		}
	})

	// A deferred (proxyless) init failure must not sink the whole instance
	// when another transport is configured: kindling degrades to the working
	// transports rather than failing construction.
	t.Run("ProxylessFails_OtherTransportSurvives", func(t *testing.T) {
		orig := newSmartDialerFn
		newSmartDialerFn = func(_ io.Writer, _ []byte, _ transport.StreamDialer, _ transport.PacketDialer, _ ...string) (transport.StreamDialer, error) {
			return nil, errors.New("probe failed")
		}
		t.Cleanup(func() { newSmartDialerFn = orig })

		k, err := NewKindling("test",
			WithTransport(&namedTransport{name: "stub"}),
			WithProxyless("example.com"),
		)
		if err != nil {
			t.Fatalf("NewKindling() error = %v; want nil when a non-proxyless transport remains", err)
		}
		ki := k.(*kindling)
		if len(ki.transports) != 1 || ki.transports[0].Name() != "stub" {
			t.Errorf("transports = %v; want only the surviving %q transport", ki.transports, "stub")
		}
	})

	// When the only configured transport is proxyless and its deferred init
	// fails, kindling has zero usable transports and must surface that at
	// construction rather than deferring a "no eligible transports" error to
	// the first request.
	t.Run("ProxylessFails_NoOtherTransport_ReturnsError", func(t *testing.T) {
		orig := newSmartDialerFn
		newSmartDialerFn = func(_ io.Writer, _ []byte, _ transport.StreamDialer, _ transport.PacketDialer, _ ...string) (transport.StreamDialer, error) {
			return nil, errors.New("probe failed")
		}
		t.Cleanup(func() { newSmartDialerFn = orig })

		_, err := NewKindling("test", WithProxyless("example.com"))
		if err == nil {
			t.Fatal("NewKindling() = nil error; want error when no transports remain")
		}
		if !strings.Contains(err.Error(), "probe failed") {
			t.Errorf("error = %q; want it to wrap the underlying deferred failure", err)
		}
	})

	// WithSmartDialerConfig must reach newSmartDialerFn whether it was set
	// before or after WithProxyless. Guards the deferred-construction path
	// from regressing.
	t.Run("ProxylessConfigOrderIndependent", func(t *testing.T) {
		customCfg := []byte("dns: []\ntls: []\n")
		cases := []struct {
			name string
			opts []Option
		}{
			{"config-before-proxyless", []Option{
				WithSmartDialerConfig(customCfg),
				WithProxyless("example.com"),
			}},
			{"proxyless-before-config", []Option{
				WithProxyless("example.com"),
				WithSmartDialerConfig(customCfg),
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var gotCfg []byte
				orig := newSmartDialerFn
				newSmartDialerFn = func(_ io.Writer, cfg []byte, _ transport.StreamDialer, _ transport.PacketDialer, _ ...string) (transport.StreamDialer, error) {
					gotCfg = cfg
					return stubStreamDialer{}, nil
				}
				t.Cleanup(func() { newSmartDialerFn = orig })

				if _, err := NewKindling("test", tc.opts...); err != nil {
					t.Fatalf("NewKindling() error = %v", err)
				}
				if string(gotCfg) != string(customCfg) {
					t.Errorf("newSmartDialer config arg = %q; want %q", gotCfg, customCfg)
				}
			})
		}
	})

	t.Run("WithSmartDialerConfig_Empty_ReturnsError", func(t *testing.T) {
		t.Parallel()
		if _, err := NewKindling("test", WithSmartDialerConfig(nil)); err == nil {
			t.Error("NewKindling(WithSmartDialerConfig(nil)) should return error")
		}
		if _, err := NewKindling("test", WithSmartDialerConfig([]byte{})); err == nil {
			t.Error("NewKindling(WithSmartDialerConfig(empty)) should return error")
		}
	})
}

// TestNewSmartHTTPTransportWithConfig guards the standalone (non-Kindling)
// path used by radiance/kindling/smart: the config []byte must reach
// newSmartDialerFn in the same slot WithSmartDialerConfig sends it through,
// and the helper must reject a non-nil empty slice — matching the option
// path's validation so a `make([]byte, 0)` mistake doesn't silently skip
// the embedded default and hand outline-sdk empty YAML.
func TestNewSmartHTTPTransportWithConfig(t *testing.T) {
	t.Run("PlumbsConfigThrough", func(t *testing.T) {
		customCfg := []byte("dns: []\ntls: []\n")
		stream := stubStreamDialer{}
		packet := stubPacketDialer{}

		var gotCfg []byte
		var gotStream transport.StreamDialer
		var gotPacket transport.PacketDialer
		var gotDomains []string
		orig := newSmartDialerFn
		newSmartDialerFn = func(_ io.Writer, cfg []byte, s transport.StreamDialer, p transport.PacketDialer, domains ...string) (transport.StreamDialer, error) {
			gotCfg, gotStream, gotPacket, gotDomains = cfg, s, p, domains
			return stubStreamDialer{}, nil
		}
		t.Cleanup(func() { newSmartDialerFn = orig })

		_, err := NewSmartHTTPTransportWithConfig(io.Discard, customCfg, stream, packet, "example.com")
		if err != nil {
			t.Fatalf("NewSmartHTTPTransportWithConfig() error = %v", err)
		}
		if string(gotCfg) != string(customCfg) {
			t.Errorf("config arg = %q; want %q", gotCfg, customCfg)
		}
		if gotStream != stream {
			t.Errorf("stream arg = %v; want %v", gotStream, stream)
		}
		if gotPacket != packet {
			t.Errorf("packet arg = %v; want %v", gotPacket, packet)
		}
		if len(gotDomains) != 1 || gotDomains[0] != "example.com" {
			t.Errorf("domains arg = %v; want [example.com]", gotDomains)
		}
	})

	t.Run("NilConfigUsesEmbedded", func(t *testing.T) {
		var gotCfg []byte
		orig := newSmartDialerFn
		newSmartDialerFn = func(_ io.Writer, cfg []byte, _ transport.StreamDialer, _ transport.PacketDialer, _ ...string) (transport.StreamDialer, error) {
			gotCfg = cfg
			return stubStreamDialer{}, nil
		}
		t.Cleanup(func() { newSmartDialerFn = orig })

		_, err := NewSmartHTTPTransportWithConfig(io.Discard, nil, nil, nil, "example.com")
		if err != nil {
			t.Fatalf("NewSmartHTTPTransportWithConfig() error = %v", err)
		}
		if gotCfg != nil {
			t.Errorf("config arg = %q; want nil (so newSmartDialer falls back to embedded)", gotCfg)
		}
	})

	t.Run("EmptyConfigReturnsError", func(t *testing.T) {
		_, err := NewSmartHTTPTransportWithConfig(io.Discard, []byte{}, nil, nil, "example.com")
		if err == nil {
			t.Error("NewSmartHTTPTransportWithConfig(empty) should return error")
		}
	})
}

// stubStreamDialer is a minimal transport.StreamDialer for tests that just
// need to verify the dialer pointer was plumbed through. The Dial method is
// not expected to fire — assertions check struct fields, not behavior.
type stubStreamDialer struct{}

func (stubStreamDialer) DialStream(context.Context, string) (transport.StreamConn, error) {
	return nil, errors.New("stub: unused")
}

// stubPacketDialer is the UDP counterpart to stubStreamDialer.
type stubPacketDialer struct{}

func (stubPacketDialer) DialPacket(context.Context, string) (net.Conn, error) {
	return nil, errors.New("stub: unused")
}

func TestLantern(t *testing.T) {
	if os.Getenv("KINDLING_INTEGRATION") == "" {
		t.Skip("skipping integration test; set KINDLING_INTEGRATION=1 to run")
	}
	k, err := NewKindling("kindling",
		WithProxyless("config.getiantem.org"),
	)
	if err != nil {
		t.Fatalf("NewKindling() error = %v", err)
	}
	client := k.NewHTTPClient()
	r, err := newRequestWithHeaders(context.Background(), "POST", "https://config.getiantem.org:443/proxies.yaml.gz", http.NoBody)
	if err != nil {
		t.Fatalf("newRequestWithHeaders() error = %v", err)
	}
	res, err := client.Do(r)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	fmt.Printf("Response: %d bytes, status %d\n", len(body), res.StatusCode)
}

// TestLantern_DomainFronting exercises the full Kindling → domainfront path
// against real CDN infrastructure: it fetches the production fronted.yaml.gz
// (which ships both CloudFront and Akamai providers), constructs a Kindling
// with WithDomainFronting, and performs a real request through the fronted
// transport. Passes only when a front actually works end-to-end.
//
// Gated on KINDLING_INTEGRATION=1 because it hits external CDN endpoints.
func TestLantern_DomainFronting(t *testing.T) {
	if os.Getenv("KINDLING_INTEGRATION") == "" {
		t.Skip("skipping integration test; set KINDLING_INTEGRATION=1 to run")
	}

	const configURL = "https://raw.githubusercontent.com/getlantern/fronted/refs/heads/main/fronted.yaml.gz"

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, configURL, nil)
	if err != nil {
		t.Fatalf("new config request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch fronted.yaml.gz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected config status: %d", resp.StatusCode)
	}
	cfgBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	cfg, err := domainfront.ParseConfig(cfgBytes)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	if _, ok := cfg.Providers["cloudfront"]; !ok {
		t.Fatalf("expected cloudfront provider in config")
	}
	if _, ok := cfg.Providers["akamai"]; !ok {
		t.Fatalf("expected akamai provider in config")
	}
	t.Logf("loaded config with %d providers", len(cfg.Providers))

	df, err := domainfront.New(ctx, cfg,
		domainfront.WithConfigURL(configURL),
	)
	if err != nil {
		t.Fatalf("domainfront.New: %v", err)
	}
	defer df.Close()

	k, err := NewKindling("kindling-integration",
		WithDomainFronting(df),
	)
	if err != nil {
		t.Fatalf("NewKindling: %v", err)
	}

	// Target a host that the production fronted.yaml.gz maps to a real CDN
	// distribution. config.getiantem.org is the canonical mapped origin that
	// is used in production.
	client := k.NewHTTPClient()
	r, err := newRequestWithHeaders(ctx, http.MethodPost, "https://config.getiantem.org/proxies.yaml.gz", http.NoBody)
	if err != nil {
		t.Fatalf("newRequestWithHeaders: %v", err)
	}
	res, err := client.Do(r)
	if err != nil {
		t.Fatalf("client.Do via domain fronting: %v", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	t.Logf("domainfront response: status=%d bodyLen=%d", res.StatusCode, len(body))

	// Status-code assertions only: a successful domainfront request can legitimately
	// return an empty body (204, 304, a 405 with no body, etc). The status codes
	// below are the ones that indicate the CDN rejected us (400/403) or the
	// origin failed (5xx) — anything else means we tunneled through.
	if res.StatusCode >= 500 {
		t.Fatalf("origin returned server error: %d", res.StatusCode)
	}
	if res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusForbidden {
		t.Fatalf("CDN rejected request (status %d) — domain fronting failed", res.StatusCode)
	}
}

const (
	appVersionHeader = "X-Lantern-App-Version"
	versionHeader    = "X-Lantern-Version"
	platformHeader   = "X-Lantern-Platform"
	appNameHeader    = "X-Lantern-App"
	deviceIdHeader   = "X-Lantern-Device-Id"
	userIdHeader     = "X-Lantern-User-Id"
)

const (
	AppName       = "kindling"
	ClientVersion = "7.6.47"
	Version       = "7.6.47"
	Platform      = "linux"
	DeviceId      = "some-uuid-here"
	UserId        = "23409"
	ProToken      = ""
)

func newRequestWithHeaders(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(appVersionHeader, ClientVersion)
	req.Header.Set(versionHeader, Version)
	req.Header.Set(userIdHeader, UserId)
	req.Header.Set(platformHeader, Platform)
	req.Header.Set(appNameHeader, AppName)
	req.Header.Set(deviceIdHeader, DeviceId)
	return req, nil
}

type dummyRoundTripper struct{}

func (d *dummyRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

func TestKindling_Suite(t *testing.T) {
	t.Parallel()

	t.Run("NewKindling", func(t *testing.T) {
		t.Parallel()
		k, err := NewKindling("kindling")
		if err != nil {
			t.Fatalf("NewKindling() error = %v", err)
		}
		if k == nil {
			t.Error("NewKindling() = nil; want non-nil")
		}
	})

	t.Run("NewHTTPClient", func(t *testing.T) {
		t.Parallel()
		k, err := NewKindling("kindling")
		if err != nil {
			t.Fatalf("NewKindling() error = %v", err)
		}
		client := k.NewHTTPClient()
		if client == nil {
			t.Error("NewHTTPClient() = nil; want non-nil")
		}
	})
}

func TestReplaceTransport(t *testing.T) {
	t.Parallel()

	makeTransport := func(name string) Transport {
		return &namedTransport{
			name:         name,
			maxLength:    100,
			isStreamable: true,
			newRT: func(ctx context.Context, addr string) (http.RoundTripper, error) {
				return &dummyRoundTripper{}, nil
			},
		}
	}

	t.Run("Success", func(t *testing.T) {
		t.Parallel()
		k, err := NewKindling("test", WithTransport(makeTransport("test-transport")))
		if err != nil {
			t.Fatal(err)
		}
		err = k.ReplaceTransport("test-transport", func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		})
		if err != nil {
			t.Errorf("ReplaceTransport() error = %v; want nil", err)
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		t.Parallel()
		k, err := NewKindling("test", WithTransport(makeTransport("test-transport")))
		if err != nil {
			t.Fatal(err)
		}
		err = k.ReplaceTransport("nonexistent", func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		})
		if err == nil {
			t.Error("ReplaceTransport() should fail for unknown transport")
		}
		if !strings.Contains(err.Error(), "nonexistent") {
			t.Errorf("error should mention transport name, got: %v", err)
		}
	})

	t.Run("Multiple", func(t *testing.T) {
		t.Parallel()
		k, err := NewKindling("test",
			WithTransport(makeTransport("transport-1")),
			WithTransport(makeTransport("transport-2")),
		)
		if err != nil {
			t.Fatal(err)
		}
		err = k.ReplaceTransport("transport-2", func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		})
		if err != nil {
			t.Errorf("ReplaceTransport() error = %v; want nil", err)
		}
	})

	t.Run("EmptyTransportList", func(t *testing.T) {
		t.Parallel()
		k, err := NewKindling("test")
		if err != nil {
			t.Fatal(err)
		}
		err = k.ReplaceTransport("any", func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		})
		if err == nil {
			t.Error("ReplaceTransport() should fail with no transports")
		}
	})
}
