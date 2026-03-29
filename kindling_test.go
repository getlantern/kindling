package kindling

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/getlantern/fronted"
)

func TestNewKindling(t *testing.T) {
	t.Parallel()

	t.Run("Success", func(t *testing.T) {
		t.Parallel()

		f := fronted.NewFronted(
			fronted.WithConfigURL("https://media.githubusercontent.com/media/getlantern/fronted/refs/heads/main/fronted.yaml.gz"))
		k, err := NewKindling("kindling",
			WithDomainFronting(f),
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
