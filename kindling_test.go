package kindling

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
)

func TestNewKindling(t *testing.T) {
	t.Parallel()

	t.Run("Success", func(t *testing.T) {
		t.Parallel()

		kindling := NewKindling(
			WithDomainFronting("https://media.githubusercontent.com/media/getlantern/fronted/refs/heads/main/fronted.yaml.gz", ""),
			WithPanicListener(func(string) {}),
		)
		if kindling == nil {
			t.Errorf("NewKindling() = nil; want non-nil Kindling")
		}
	})
}

func TestLantern(t *testing.T) {
	k := NewKindling(
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

func TestKindling(t *testing.T) {
	t.Parallel()

	t.Run("NewKindling", func(t *testing.T) {
		t.Parallel()

		t.Run("Success", func(t *testing.T) {
			t.Parallel()

			kindling := NewKindling()
			if kindling == nil {
				t.Errorf("NewKindling() = nil; want non-nil Kindling")
			}
		})
	})

	t.Run("kindling.NewHTTPClient", func(t *testing.T) {
		t.Parallel()

		t.Run("Success", func(t *testing.T) {
			t.Parallel()

			kindling := NewKindling()
			httpClient := kindling.NewHTTPClient()
			if httpClient == nil {
				t.Errorf("kindling.NewHTTPClient() = nil; want non-nil *http.Client")
			}
		})
	})
}
