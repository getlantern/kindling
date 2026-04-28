package kindling

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

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
