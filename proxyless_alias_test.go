package kindling

import (
	"context"
	"crypto/tls"
	"errors"
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
	for _, m := range []map[string]string{
		{"": "alias.example"},
		{"origin.example": ""},
		{"Origin.Example": "a.example", "origin.example": "b.example"},
		{"origin.example": "alias.example:8443"},
		{"origin.example:443": "alias.example"},
		{"origin.example": "[2001:db8::1]"},
	} {
		if _, err := NewKindling("test", WithProxylessAliases(m), WithProxyless("alias.example")); err == nil {
			t.Errorf("WithProxylessAliases(%v): want error", m)
		}
	}
}

// The alias must also be the TLS server name, or the blocked origin still shows
// up in the ClientHello. The client doesn't trust the test cert, so the request
// fails after the handshake starts; the server records the SNI either way.
func TestWithProxylessAliases_TLSServerName(t *testing.T) {
	sni := make(chan string, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		select {
		case sni <- hello.ServerName:
		default:
		}
		return nil, nil
	}}
	srv.StartTLS()
	defer srv.Close()

	dialer := &recordingDialer{srv: srv.Listener.Addr().String()}
	stubSearch(t, func(string) (transport.StreamDialer, error) { return dialer, nil })
	k, err := NewKindling("test",
		WithProxyless("alias.example"),
		WithProxylessAliases(map[string]string{"origin.example": "alias.example"}),
	)
	if err != nil {
		t.Fatalf("NewKindling() error = %v", err)
	}
	if resp, err := k.NewHTTPClient().Get("https://origin.example/v1/config"); err == nil {
		resp.Body.Close()
	}
	select {
	case got := <-sni:
		if got != "alias.example" {
			t.Errorf("TLS server name = %q, want alias.example", got)
		}
	default:
		t.Fatal("server saw no TLS handshake")
	}
}

func TestHostRewriter_IPv6AliasWithoutPort(t *testing.T) {
	var got string
	rt := &hostRewriter{host: "2001:db8::1", next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.URL.Host
		return nil, io.EOF
	})}
	req, _ := http.NewRequest(http.MethodGet, "https://origin.example/v1", nil)
	rt.RoundTrip(req)
	if got != "[2001:db8::1]" {
		t.Errorf("URL.Host = %q, want [2001:db8::1]", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// An alias target in different case from WithProxyless must still use its own
// strategy search, not the first domain's.
func TestWithProxylessAliases_TargetCaseSelectsSearch(t *testing.T) {
	var mu sync.Mutex
	var searched []string
	stubSearch(t, func(domain string) (transport.StreamDialer, error) {
		mu.Lock()
		searched = append(searched, domain)
		mu.Unlock()
		return dialedStreamDialer{}, nil
	})
	k, err := NewKindling("test",
		WithProxyless("other.example", "alias.example"),
		WithProxylessAliases(map[string]string{"origin.example": "ALIAS.EXAMPLE"}),
	)
	if err != nil {
		t.Fatalf("NewKindling() error = %v", err)
	}
	if err := dialSmart(t, k, "origin.example:443"); !errors.Is(err, errStubDialed) {
		t.Fatalf("dial error = %v; want %v", err, errStubDialed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(searched) != 1 || searched[0] != "alias.example" {
		t.Errorf("searched = %v; want only [alias.example]", searched)
	}
}
