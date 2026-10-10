package dnstunnel

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
)

// maxPoolHosts caps CIDR expansion so a wide range can't balloon memory.
const maxPoolHosts = 256

const (
	lossDecay      = 0.98
	minSamples     = 5.0
	disableLoss    = 0.9
	reenableAfter  = 30_000
	failoverStreak = 3
)

type resolver struct {
	addr       netip.AddrPort
	srtt       float64
	hasRTT     bool
	loss       float64
	total      float64
	enabled    bool
	disabledAt uint64
}

func (r *resolver) lossRatio() float64 {
	if r.total <= 0 {
		return 0
	}
	return r.loss / r.total
}

// score ranks resolvers (lower is better): loss dominates, RTT breaks ties.
func (r *resolver) score() float64 {
	rtt := 1000.0
	if r.hasRTT {
		rtt = r.srtt
	}
	return r.lossRatio()*10_000 + rtt
}

// resolverPool spreads queries across recursive resolvers: a sticky preferred resolver plus
// duplicates to the next-healthiest, with decayed loss tracking, auto-disable and failover.
type resolverPool struct {
	rs          []*resolver
	duplication int
	sticky      int
	streak      int
	rr          int
}

// parseResolvers accepts IP, IP:port, [v6]:port, CIDR and CIDR:port; the default port is 53.
func parseResolvers(specs []string, duplication int) (*resolverPool, error) {
	p := &resolverPool{duplication: max(duplication, 1)}
	seen := make(map[netip.AddrPort]bool)
	add := func(ap netip.AddrPort) {
		if !seen[ap] && len(p.rs) < maxPoolHosts {
			seen[ap] = true
			p.rs = append(p.rs, &resolver{addr: ap, enabled: true})
		}
	}
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		if ap, err := netip.ParseAddrPort(spec); err == nil {
			add(ap)
			continue
		}
		if a, err := netip.ParseAddr(spec); err == nil {
			add(netip.AddrPortFrom(a, 53))
			continue
		}
		prefixStr, port := spec, uint16(53)
		if i := strings.LastIndex(spec, ":"); i > 0 && strings.Contains(spec, "/") && strings.Count(spec, ":") == 1 {
			n, err := strconv.ParseUint(spec[i+1:], 10, 16)
			if err != nil {
				return nil, fmt.Errorf("dnstunnel: bad port in resolver %q", spec)
			}
			prefixStr, port = spec[:i], uint16(n)
		}
		prefix, err := netip.ParsePrefix(prefixStr)
		if err != nil {
			return nil, fmt.Errorf("dnstunnel: bad resolver %q", spec)
		}
		prefix = prefix.Masked()
		for a := prefix.Addr(); prefix.Contains(a) && len(p.rs) < maxPoolHosts; a = a.Next() {
			add(netip.AddrPortFrom(a, port))
		}
	}
	if len(p.rs) == 0 {
		return nil, errors.New("dnstunnel: no resolvers")
	}
	return p, nil
}

// systemResolvers reads /etc/resolv.conf nameservers. Mobile platforms have no such file; they
// must supply resolvers via Config.Resolvers.
func systemResolvers() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			if a, err := netip.ParseAddr(fields[1]); err == nil {
				out = append(out, netip.AddrPortFrom(a, 53).String()) // keeps a link-local zone
			}
		}
	}
	return out
}

func (p *resolverPool) reactivate(now uint64) {
	for _, r := range p.rs {
		if !r.enabled && now-min(now, r.disabledAt) >= reenableAfter {
			r.enabled, r.loss, r.total = true, 0, 0
		}
	}
}

func (p *resolverPool) ranked() []int {
	var idx []int
	for i, r := range p.rs {
		if r.enabled {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return p.rs[idx[a]].score() < p.rs[idx[b]].score() })
	return idx
}

func (p *resolverPool) len() int { return len(p.rs) }

// probeTargets returns up to n enabled resolvers, healthiest first: the ones failover can reach.
func (p *resolverPool) probeTargets(n int) []netip.AddrPort {
	var out []netip.AddrPort
	for _, i := range p.ranked() {
		if len(out) == n {
			break
		}
		out = append(out, p.rs[i].addr)
	}
	return out
}

// pick chooses the resolver(s) for the next query: the sticky one plus duplicates.
func (p *resolverPool) pick(now uint64) []netip.AddrPort {
	p.reactivate(now)
	ranked := p.ranked()
	if len(ranked) == 0 {
		// Better to try a bad resolver than stall.
		for _, r := range p.rs {
			r.enabled = true
		}
		ranked = p.ranked()
	}
	if !p.rs[p.sticky].enabled {
		p.sticky = ranked[0]
	}
	want := min(p.duplication, len(ranked))
	out := make([]netip.AddrPort, 0, want)
	out = append(out, p.rs[p.sticky].addr)
	for k := 0; k < len(ranked) && len(out) < want; k++ {
		a := p.rs[ranked[(p.rr+k)%len(ranked)]].addr
		if !containsAddr(out, a) {
			out = append(out, a)
		}
	}
	p.rr++
	return out
}

func containsAddr(s []netip.AddrPort, a netip.AddrPort) bool {
	for _, v := range s {
		if v == a {
			return true
		}
	}
	return false
}

func (p *resolverPool) index(a netip.AddrPort) int {
	for i, r := range p.rs {
		if r.addr == a {
			return i
		}
	}
	return -1
}

func (p *resolverPool) onSuccess(a netip.AddrPort, rttMS uint64) {
	i := p.index(a)
	if i < 0 {
		return
	}
	r := p.rs[i]
	r.loss *= lossDecay
	r.total = r.total*lossDecay + 1
	if r.hasRTT {
		r.srtt = 0.875*r.srtt + 0.125*float64(rttMS)
	} else {
		r.srtt, r.hasRTT = float64(rttMS), true
	}
	if i == p.sticky {
		p.streak = 0
	}
}

func (p *resolverPool) onLoss(addrs []netip.AddrPort, now uint64) {
	stickyHit := false
	for _, a := range addrs {
		i := p.index(a)
		if i < 0 {
			continue
		}
		r := p.rs[i]
		r.loss = r.loss*lossDecay + 1
		r.total = r.total*lossDecay + 1
		if r.enabled && r.total >= minSamples && r.lossRatio() >= disableLoss {
			r.enabled, r.disabledAt = false, now
		}
		if i == p.sticky {
			stickyHit = true
		}
	}
	if !stickyHit {
		return
	}
	p.streak++
	if p.streak >= failoverStreak || !p.rs[p.sticky].enabled {
		p.failover(now)
	}
}

// failover moves the sticky preference to the healthiest other enabled resolver.
func (p *resolverPool) failover(now uint64) {
	p.reactivate(now)
	best := -1
	for i, r := range p.rs {
		if i != p.sticky && r.enabled && (best < 0 || r.score() < p.rs[best].score()) {
			best = i
		}
	}
	if best >= 0 {
		p.sticky = best
	}
	p.streak = 0
}
