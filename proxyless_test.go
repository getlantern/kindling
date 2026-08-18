package kindling

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
)

// errStubDialed marks "the strategy search finished and its dialer was used",
// which is otherwise indistinguishable from a search failure at the call site.
var errStubDialed = errors.New("stub: dialed")

type dialedStreamDialer struct{}

func (dialedStreamDialer) DialStream(context.Context, string) (transport.StreamConn, error) {
	return nil, errStubDialed
}

var errNoSmartTransport = errors.New("no smart transport registered")

// dialSmart drives the proxyless transport, which is what triggers its lazy
// strategy search, and returns the resulting error. Safe to call from a
// spawned goroutine — it reports failures as errors rather than via t.Fatalf.
func dialSmart(t *testing.T, k Kindling, addr string) error {
	t.Helper()
	return dialSmartCtx(t, context.Background(), k, addr)
}

func dialSmartCtx(t *testing.T, ctx context.Context, k Kindling, addr string) error {
	t.Helper()
	for _, tr := range k.(*kindling).transports {
		if tr.Name() == string(TransportSmart) {
			_, err := tr.NewRoundTripper(ctx, addr)
			return err
		}
	}
	return errNoSmartTransport
}

// stubSearch swaps the strategy search for fn and restores it after the test.
//
// A test whose fn blocks must ensure every search it started has entered fn
// before returning: an abandoned search keeps running, and the restore here
// would otherwise race its read of the seam.
func stubSearch(t *testing.T, fn func(domain string) (transport.StreamDialer, error)) {
	t.Helper()
	orig := newSmartDialerFn
	newSmartDialerFn = func(
		_ context.Context,
		_ io.Writer,
		_ []byte,
		_ transport.StreamDialer,
		_ transport.PacketDialer,
		domains ...string,
	) (transport.StreamDialer, error) {
		// An error rather than t.Errorf and a fallthrough: indexing domains[0]
		// on an empty slice would panic and bury whatever really went wrong.
		if len(domains) != 1 {
			return nil, fmt.Errorf("search got %d domains, want exactly 1 (searches are per domain): %v",
				len(domains), domains)
		}
		return fn(domains[0])
	}
	t.Cleanup(func() { newSmartDialerFn = orig })
}

// A censored network turns the strategy search into seconds of live probing.
// Construction must not pay for it: callers on a start deadline (the iOS
// NEPacketTunnelProvider, ~7.5s) were killed mid-search — engineering#3822.
func TestWithProxyless_SlowSearchDoesNotBlockConstruction(t *testing.T) {
	const searchDelay = 3 * time.Second
	stubSearch(t, func(string) (transport.StreamDialer, error) {
		time.Sleep(searchDelay)
		return dialedStreamDialer{}, nil
	})

	start := time.Now()
	k, err := NewKindling("test", WithProxyless("example.com"))
	if err != nil {
		t.Fatalf("NewKindling() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > searchDelay/4 {
		t.Fatalf("NewKindling() took %v with a %v search; want it to return without waiting", elapsed, searchDelay)
	}

	// The search still has to produce a working dialer, just off the
	// construction path.
	if err := dialSmart(t, k, "example.com:443"); !errors.Is(err, errStubDialed) {
		t.Errorf("dial error = %v; want %v", err, errStubDialed)
	}
}

// A caller that cannot wait out the search must be able to give up on its own
// context instead of inheriting the search's timeline.
func TestWithProxyless_DialRespectsCallerDeadline(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	stubSearch(t, func(string) (transport.StreamDialer, error) {
		close(entered)
		<-release
		return dialedStreamDialer{}, nil
	})

	k, err := NewKindling("test", WithProxyless("example.com"))
	if err != nil {
		t.Fatalf("NewKindling() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := dialSmartCtx(t, ctx, k, "example.com:443"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("dial error = %v; want %v", err, context.DeadlineExceeded)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("dial took %v; want it bounded by the caller's 50ms deadline", elapsed)
	}

	// The abandoned search runs on regardless — that is the design. Let it
	// finish before the seam is restored.
	<-entered
	close(release)
}

// The search is conjunctive, so one dialer covering several domains fails
// whenever any single domain is blocked. Searching per domain keeps a blocked
// api.getiantem.org from taking a reachable df.iantem.io down with it.
func TestWithProxyless_BlockedDomainDoesNotSinkSibling(t *testing.T) {
	stubSearch(t, func(domain string) (transport.StreamDialer, error) {
		if domain == "blocked.example" {
			return nil, errors.New("probe failed")
		}
		return dialedStreamDialer{}, nil
	})

	k, err := NewKindling("test", WithProxyless("reachable.example", "blocked.example"))
	if err != nil {
		t.Fatalf("NewKindling() error = %v", err)
	}

	if err := dialSmart(t, k, "reachable.example:443"); !errors.Is(err, errStubDialed) {
		t.Errorf("reachable dial error = %v; want %v", err, errStubDialed)
	}
	if err := dialSmart(t, k, "blocked.example:443"); err == nil || !strings.Contains(err.Error(), "probe failed") {
		t.Errorf("blocked dial error = %v; want the search failure", err)
	}
}

// Concurrent dials must share one search: a search probes the whole strategy
// space, and several at once overran the iOS extension's 50 MB jetsam cap.
func TestWithProxyless_SearchIsSingleFlight(t *testing.T) {
	var searches atomic.Int32
	release := make(chan struct{})
	stubSearch(t, func(string) (transport.StreamDialer, error) {
		searches.Add(1)
		<-release
		return dialedStreamDialer{}, nil
	})

	k, err := NewKindling("test", WithProxyless("example.com"))
	if err != nil {
		t.Fatalf("NewKindling() error = %v", err)
	}

	const dialers = 8
	dispatched := make(chan struct{}, dialers)
	var wg sync.WaitGroup
	errs := make([]error, dialers)
	for i := range dialers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dispatched <- struct{}{}
			errs[i] = dialSmart(t, k, "example.com:443")
		}()
	}
	// Wait for every dial to be dispatched rather than sleeping a fixed span.
	// The assertion holds either way: a dial that lands after the search has
	// finished gets the cached dialer, so it still starts no second search.
	for range dialers {
		<-dispatched
	}
	close(release)
	wg.Wait()

	if got := searches.Load(); got != 1 {
		t.Errorf("searches = %d; want 1 shared by all %d dials", got, dialers)
	}
	for i, err := range errs {
		if !errors.Is(err, errStubDialed) {
			t.Errorf("dial %d error = %v; want %v", i, err, errStubDialed)
		}
	}
}

// A successful search is reused; a failed one is deliberately forgotten so a
// domain blocked at first contact recovers without rebuilding kindling.
func TestWithProxyless_FailedSearchRetriesAndSuccessCaches(t *testing.T) {
	var searches atomic.Int32
	stubSearch(t, func(string) (transport.StreamDialer, error) {
		if searches.Add(1) <= 2 {
			return nil, errors.New("probe failed")
		}
		return dialedStreamDialer{}, nil
	})

	k, err := NewKindling("test", WithProxyless("example.com"))
	if err != nil {
		t.Fatalf("NewKindling() error = %v", err)
	}

	for i := range 2 {
		if err := dialSmart(t, k, "example.com:443"); err == nil || !strings.Contains(err.Error(), "probe failed") {
			t.Fatalf("dial %d error = %v; want the search failure", i, err)
		}
	}
	if got := searches.Load(); got != 2 {
		t.Fatalf("searches after two failed dials = %d; want 2 (a failure must not be cached)", got)
	}

	for i := range 2 {
		if err := dialSmart(t, k, "example.com:443"); !errors.Is(err, errStubDialed) {
			t.Fatalf("dial %d after recovery: error = %v; want %v", i, err, errStubDialed)
		}
	}
	if got := searches.Load(); got != 3 {
		t.Errorf("searches after recovery = %d; want 3 (a success must be cached)", got)
	}
}

// An address with no matching search falls back to the first configured
// domain's dialer rather than failing outright.
func TestWithProxyless_UnknownHostUsesFirstDomain(t *testing.T) {
	var searched []string
	var mu sync.Mutex
	stubSearch(t, func(domain string) (transport.StreamDialer, error) {
		mu.Lock()
		searched = append(searched, domain)
		mu.Unlock()
		return dialedStreamDialer{}, nil
	})

	k, err := NewKindling("test", WithProxyless("first.example", "second.example"))
	if err != nil {
		t.Fatalf("NewKindling() error = %v", err)
	}
	if err := dialSmart(t, k, "unlisted.example:443"); !errors.Is(err, errStubDialed) {
		t.Fatalf("dial error = %v; want %v", err, errStubDialed)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(searched) != 1 || searched[0] != "first.example" {
		t.Errorf("searched = %v; want only [first.example]", searched)
	}
}

func TestWithProxyless_NoDomains_ReturnsError(t *testing.T) {
	if _, err := NewKindling("test", WithProxyless()); err == nil {
		t.Error("NewKindling(WithProxyless()) = nil error; want an error")
	}
}
