package kindling

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
)

// smartSearchTimeout bounds one strategy search. The Outline finder applies
// only a per-probe TestTimeout, so without an overall bound a wedged probe set
// pins the search goroutine for the life of the process.
const smartSearchTimeout = 60 * time.Second

// lazySmartDialer dials through one Outline smart dialer per domain, each built
// by a strategy search that starts on first dial rather than at construction.
//
// One search per domain rather than one covering all of them: the search is
// conjunctive, so a single dialer given several domains succeeds only if one
// strategy unblocks every one of them, and a domain that is blocked outright
// then takes the reachable ones down with it.
type lazySmartDialer struct {
	// domains is in caller order and deduplicated; domains[0] is the fallback
	// for addresses that match no configured domain.
	domains  []string
	searches map[string]*smartSearch
}

func newLazySmartDialer(
	logWriter io.Writer,
	config []byte,
	stream transport.StreamDialer,
	packet transport.PacketDialer,
	domains []string,
) *lazySmartDialer {
	l := &lazySmartDialer{searches: make(map[string]*smartSearch, len(domains))}
	for _, domain := range domains {
		if _, seen := l.searches[domain]; seen {
			continue
		}
		l.domains = append(l.domains, domain)
		l.searches[domain] = &smartSearch{
			build: func() (transport.StreamDialer, error) {
				ctx, cancel := context.WithTimeout(context.Background(), smartSearchTimeout)
				defer cancel()
				return newSmartDialerFn(ctx, logWriter, config, stream, packet, domain)
			},
		}
	}
	return l
}

// DialStream waits for the search covering addr's host, bounded by ctx, then
// dials through the dialer it produced.
func (l *lazySmartDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	search, err := l.searchFor(addr)
	if err != nil {
		return nil, err
	}
	dialer, err := search.await(ctx)
	if err != nil {
		return nil, err
	}
	return dialer.DialStream(ctx, addr)
}

// searchFor returns the search for addr's host, falling back to the first
// configured domain: a dialer tuned for a sibling host is a better guess than
// failing the dial outright.
func (l *lazySmartDialer) searchFor(addr string) (*smartSearch, error) {
	if len(l.domains) == 0 {
		return nil, errors.New("no proxyless domains configured")
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if search, ok := l.searches[host]; ok {
		return search, nil
	}
	return l.searches[l.domains[0]], nil
}

// smartSearch owns one domain's strategy search. Searches are single-flight:
// callers arriving while one runs join it instead of starting a second.
type smartSearch struct {
	build func() (transport.StreamDialer, error)

	mu       sync.Mutex
	dialer   transport.StreamDialer
	inFlight *pendingSearch
}

// pendingSearch is one attempt at building a domain's dialer. dialer and err
// are written before done is closed, so a caller that has received from done
// may read them without the lock.
type pendingSearch struct {
	done   chan struct{}
	dialer transport.StreamDialer
	err    error
}

// await returns the domain's dialer, starting a search on first use and joining
// one already under way otherwise. ctx bounds only the wait — the search runs to
// completion on its own goroutine, so work a caller gave up on still serves
// whoever comes next.
func (s *smartSearch) await(ctx context.Context) (transport.StreamDialer, error) {
	dialer, pending := s.begin()
	if dialer != nil {
		return dialer, nil
	}
	select {
	case <-pending.done:
		return pending.dialer, pending.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// begin hands the search back rather than waiting on it, so the caller that
// starts one can abandon it on its own context just as a joiner can.
func (s *smartSearch) begin() (transport.StreamDialer, *pendingSearch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dialer != nil {
		return s.dialer, nil
	}
	if s.inFlight == nil {
		s.inFlight = &pendingSearch{done: make(chan struct{})}
		go s.run(s.inFlight)
	}
	return nil, s.inFlight
}

// run remembers a successful search and deliberately forgets a failed one, so a
// domain blocked at first contact recovers on a later dial without a restart.
func (s *smartSearch) run(pending *pendingSearch) {
	defer func() {
		if r := recover(); r != nil {
			pending.dialer, pending.err = nil, fmt.Errorf("smart dialer search panicked: %v", r)
		}
		if pending.err == nil && pending.dialer == nil {
			pending.err = errors.New("smart dialer search produced no dialer")
		}
		s.mu.Lock()
		s.inFlight = nil
		if pending.err == nil {
			s.dialer = pending.dialer
		}
		s.mu.Unlock()
		close(pending.done)
	}()
	pending.dialer, pending.err = s.build()
}
