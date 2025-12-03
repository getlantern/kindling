package kindling

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/getlantern/fronted"
)

func TestNewKindling(t *testing.T) {
	t.Parallel()

	t.Run("Success", func(t *testing.T) {
		t.Parallel()

		f := fronted.NewFronted(
			fronted.WithConfigURL("https://media.githubusercontent.com/media/getlantern/fronted/refs/heads/main/fronted.yaml.gz"))
		kindling := NewKindling("kindling",
			WithDomainFronting(f),
			WithPanicListener(func(string) {}),
		)
		if kindling == nil {
			t.Errorf("NewKindling() = nil; want non-nil Kindling")
		}
	})
}

func TestLantern(t *testing.T) {
	k := NewKindling("kindling",
		//WithDomainFronting("https://media.githubusercontent.com/media/getlantern/fronted/refs/heads/main/fronted.yaml.gz", ""),
		WithProxyless("config.getiantem.org"),
	)
	client := k.NewHTTPClient()
	r, err := newRequestWithHeaders(context.Background(), "POST", "https://config.getiantem.org:443/proxies.yaml.gz", http.NoBody)
	if err != nil {
		t.Fatalf("newRequestWithHeaders() = %v; want nil", err)
	}
	fmt.Printf("Request: %v\n", r)
	res, err := client.Do(r)
	if err != nil {
		t.Fatalf("client.Post() = %v; want nil", err)
	}
	defer res.Body.Close()
	// Print the response
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() = %v; want nil", err)
	}
	fmt.Printf("Response: %s", body)

	// Print the response headers
	for k, v := range res.Header {
		fmt.Printf("%s: %s\n", k, v)
	}
	//t.Logf("Response: %s", body)

}

const (
	// Required common headers to send to the proxy server.
	appVersionHeader = "X-Lantern-App-Version"
	versionHeader    = "X-Lantern-Version"
	platformHeader   = "X-Lantern-Platform"
	appNameHeader    = "X-Lantern-App"
	deviceIdHeader   = "X-Lantern-Device-Id"
	userIdHeader     = "X-Lantern-User-Id"
)

const (
	AppName = "kindling"

	// Placeholders to use in the request headers.
	ClientVersion = "7.6.47"
	Version       = "7.6.47"

	Platform = "linux"
	DeviceId = "some-uuid-here"

	// userId and proToken will be set to actual values when user management is implemented.
	UserId   = "23409" // set to specific value so the server returns a desired config.
	ProToken = ""
)

// newRequestWithHeaders creates a new [http.Request] with the required headers.
func newRequestWithHeaders(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	// add required headers. Currently, all but the auth token are placeholders.
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

func TestKindling(t *testing.T) {
	t.Parallel()

	t.Run("NewKindling", func(t *testing.T) {
		t.Parallel()

		t.Run("Success", func(t *testing.T) {
			t.Parallel()

			kindling := NewKindling("kindling")
			if kindling == nil {
				t.Errorf("NewKindling() = nil; want non-nil Kindling")
			}
		})
	})

	t.Run("kindling.NewHTTPClient", func(t *testing.T) {
		t.Parallel()

		t.Run("Success", func(t *testing.T) {
			t.Parallel()

			kindling := NewKindling("kindling")
			httpClient := kindling.NewHTTPClient()
			if httpClient == nil {
				t.Errorf("kindling.NewHTTPClient() = nil; want non-nil *http.Client")
			}
		})
	})

	t.Run("kindling.ReplaceRoundTripGenerator", func(t *testing.T) {
		k := &kindling{
			roundTripperGenerators: []roundTripperGenerator{
				namedDialer("foo", func(ctx context.Context, addr string) (http.RoundTripper, error) {
					return &dummyRoundTripper{}, nil
				}),
				namedDialer("bar", func(ctx context.Context, addr string) (http.RoundTripper, error) {
					return &dummyRoundTripper{}, nil
				}),
			},
		}

		newRT := func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		}

		// Replace existing generator
		err := k.ReplaceRoundTripGenerator("foo", newRT)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if k.roundTripperGenerators[0].name() != "foo" {
			t.Errorf("expected name 'foo', got %q", k.roundTripperGenerators[0].name())
		}

		// Try to replace non-existent generator
		err = k.ReplaceRoundTripGenerator("baz", newRT)
		if err == nil {
			t.Errorf("expected error for non-existent generator, got nil")
		}
	})
}
