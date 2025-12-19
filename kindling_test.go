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
		fmt.Printf("Error: %v\n", err)
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
}

func TestKindling_ReplaceTransport(t *testing.T) {
	t.Parallel()

	t.Run("Success_ReplaceExistingTransport", func(t *testing.T) {
		t.Parallel()

		// Create a kindling instance with a transport
		originalRT := func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		}
		transport := newTransport("test-transport", 100, originalRT)
		kindling := NewKindling("test-app", WithTransport(transport))

		// Replace the transport
		newRT := func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		}
		err := kindling.ReplaceTransport("test-transport", newRT)
		if err != nil {
			t.Errorf("ReplaceTransport() error = %v; want nil", err)
		}
	})

	t.Run("Error_TransportNotFound", func(t *testing.T) {
		t.Parallel()

		// Create a kindling instance with a transport
		originalRT := func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		}
		transport := newTransport("test-transport", 100, originalRT)
		kindling := NewKindling("test-app", WithTransport(transport))

		// Try to replace a non-existent transport
		newRT := func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		}
		err := kindling.ReplaceTransport("non-existent-transport", newRT)
		if err == nil {
			t.Errorf("ReplaceTransport() error = nil; want error")
		}
		expectedError := "Could not find matching transport: non-existent-transport"
		if err.Error() != expectedError {
			t.Errorf("ReplaceTransport() error = %v; want %v", err.Error(), expectedError)
		}
	})

	t.Run("Success_ReplaceMultipleTransports", func(t *testing.T) {
		t.Parallel()

		// Create a kindling instance with multiple transports
		rt1 := func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		}
		rt2 := func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		}
		transport1 := newTransport("transport-1", 100, rt1)
		transport2 := newTransport("transport-2", 200, rt2)
		kindling := NewKindling("test-app", WithTransport(transport1), WithTransport(transport2))

		// Replace the second transport
		newRT := func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		}
		err := kindling.ReplaceTransport("transport-2", newRT)
		if err != nil {
			t.Errorf("ReplaceTransport() error = %v; want nil", err)
		}
	})

	t.Run("Success_ReplaceEmptyTransportList", func(t *testing.T) {
		t.Parallel()

		// Create a kindling instance with no transports
		kindling := NewKindling("test-app")

		// Try to replace a transport
		newRT := func(ctx context.Context, addr string) (http.RoundTripper, error) {
			return &dummyRoundTripper{}, nil
		}
		err := kindling.ReplaceTransport("any-transport", newRT)
		if err == nil {
			t.Errorf("ReplaceTransport() error = nil; want error")
		}
	})
}
