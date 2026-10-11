package kindling

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Jigsaw-Code/outline-sdk/transport"
)

// recordingDialer connects every dial to srv and records the address asked for.
type recordingDialer struct {
	srv   string
	mu    sync.Mutex
	addrs []string
}

func (d *recordingDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", d.srv)
	if err != nil {
		return nil, err
	}
	return conn.(*net.TCPConn), nil
}

func TestWithProxylessAliases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Host+r.URL.Path)
	}))
	defer srv.Close()

	dialer := &recordingDialer{srv: srv.Listener.Addr().String()}
	var searched []string
	var mu sync.Mutex
	stubSearch(t, func(domain string) (transport.StreamDialer, error) {
		mu.Lock()
		searched = append(searched, domain)
		mu.Unlock()
		return dialer, nil
	})

	aliases := WithProxylessAliases(map[string]string{"Origin.Example": "alias.example"})
	proxyless := WithProxyless("alias.example", "other.example")
	// Aliases are read when proxyless is built, so option order must not matter.
	for _, opts := range [][]Option{{aliases, proxyless}, {proxyless, aliases}} {
		k, err := NewKindling("test", opts...)
		if err != nil {
			t.Fatalf("NewKindling() error = %v", err)
		}
		checkAliases(t, k.NewHTTPClient(), dialer)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, d := range searched {
		if d == "origin.example" {
			t.Errorf("searched the aliased origin; searches = %v", searched)
		}
	}
}

func checkAliases(t *testing.T, client *http.Client, dialer *recordingDialer) {
	t.Helper()
	for _, tc := range []struct{ url, wantBody, wantDial string }{
		{"http://origin.example/v1/config", "alias.example/v1/config", "alias.example:80"},
		{"http://ORIGIN.example/v1/config", "alias.example/v1/config", "alias.example:80"},
		{"http://other.example/plans", "other.example/plans", "other.example:80"},
	} {
		req, err := http.NewRequest(http.MethodGet, tc.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		origHost := req.URL.Host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.url, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != tc.wantBody {
			t.Errorf("GET %s: server saw %q, want %q", tc.url, body, tc.wantBody)
		}
		if req.Host != origHost || req.URL.Host != origHost {
			t.Errorf("GET %s: caller's request was modified: URL.Host=%q Host=%q", tc.url, req.URL.Host, req.Host)
		}
		dialer.mu.Lock()
		last := dialer.addrs[len(dialer.addrs)-1]
		dialer.mu.Unlock()
		if last != tc.wantDial {
			t.Errorf("GET %s: dialed %q, want %q", tc.url, last, tc.wantDial)
		}
	}
}

func TestWithProxylessAliases_EmptyHost(t *testing.T) {
	for _, m := range []map[string]string{{"": "alias.example"}, {"origin.example": ""}} {
		if _, err := NewKindling("test", WithProxylessAliases(m), WithProxyless("alias.example")); err == nil {
			t.Errorf("WithProxylessAliases(%v): want error", m)
		}
	}
}
