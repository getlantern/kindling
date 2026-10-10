# Kindling
Library using a series of redundant techniques to send and receive small amounts of data through censoring firewalls. This is ideal for accessing things like configuration files during the bootrapping phase as circumvention tools first start. Kindling is intended to be used by any circumvention tool written in Go that need to reliably fetch configuration data on startup. It is also designed to be easy for any developer to add a new technique that other tools may benefit from.

The techniques integrated include:

1) [Domain fronting](https://en.wikipedia.org/wiki/Domain_fronting).
2) [Proxyless dialing from the Outline SDK](https://github.com/Jigsaw-Code/outline-sdk/tree/main/x/smart) that generally bypasses DNS-based and SNI-based blocking (i.e. works particularly well for broadly used services with a lot of IPs that are not IP-blocked)
3) DNS tunneling via [DNSTT](https://www.bamsoftware.com/software/dnstt/), or via spark's DNS-tunnel protocol (the `dnstunnel` package)
4) AMP caching also via David Fifield with a [Lantern implementation](https://github.com/getlantern/amp).

The idea is to continually add more techniques as they become available such that all tools have access to the most robust library possible for getting on the network quickly and reliably.

## Transport racing and priorities

Kindling races the configured transports against each other and returns the first usable response. Transports race in priority tiers: every transport in the default tier connects in parallel, and a lower-priority tier is dialed only once every transport in the higher-priority tiers has failed to produce a usable response.

DNS tunneling (`WithDNSTunnel`) is registered as a **last resort**. It keeps working under heavy censorship but is slow and low-throughput, so it is only dialed when the faster transports (domain fronting, proxyless dialing, AMP caching) are all blocked. Custom transports added via `WithTransport` default to the top tier; a transport can opt into a later tier by implementing `Priority() int` (higher numbers race later).

### `dnstunnel`: spark's DNS tunnel in Go

`dnstunnel` is a Go client for spark's DNS-tunnel protocol, wire-compatible with spark's `dns-tunnel-server`. It plugs into the same last-resort slot:

```go
dt, _ := dnstunnel.New(dnstunnel.Config{
    Zone:            "t.example.com",     // the NS-delegated tunnel zone
    ServerPublicKey: serverEd25519PubB64, // not a secret
    Resolvers:       platformDNSServers,  // required on mobile, which has no /etc/resolv.conf
})
k, _ := kindling.NewKindling("myapp", /* faster transports */, kindling.WithDNSTunnel(dt))
```

Targets are sent as domains and resolved by the tunnel exit, and TLS runs end to end through the tunnel. It is sized for small bootstrap requests on memory-constrained platforms such as the iOS network extension: one session at a time, built on first use and torn down when idle, with every buffer bounded. Set `ListenPacket` to protect its UDP socket from a VPN route.

The end-to-end tests run against the real server: `DNSTUNNEL_SERVER_BIN=/path/to/dns-tunnel-server go test ./dnstunnel/`.

## Example

```go
cfg, _ := domainfront.ParseConfigFromFile("fronted.yaml.gz")
df, _ := domainfront.New(ctx, cfg,
    domainfront.WithConfigURL("https://raw.githubusercontent.com/getlantern/fronted/refs/heads/main/fronted.yaml.gz"),
)
defer df.Close()

k, _ := kindling.NewKindling(
    "myapp",
    kindling.WithDomainFronting(df),
    kindling.WithProxyless("raw.githubusercontent.com"),
    kindling.WithDNSTunnel(newDNSTT()),
    kindling.WithAMPCache(ampClient),
)
httpClient := k.NewHTTPClient()
```

You can also dynamically add transports that provide a simple `Transport` interface:

```go
// Transport provides the basic interface that any transport must implement to be used by Kindling.
type Transport interface {
	// NewRoundTripper creates a new http.RoundTripper that uses this transport. As much as possible
	// the RoundTripper should be pre-connected when it is returned, as otherwise it can take too
	// much time away from other transports. In other words, Kindling parallelizes the connection
	// of the transports, but the actual sending of the request is done serially to avoid
	// issues with non-idempotent requests.
	NewRoundTripper(ctx context.Context, addr string) (http.RoundTripper, error)

	// MaxLength returns the maximum length of data that can be sent using this transport, if any.
	// A value of 0 means there is no limit.
	MaxLength() int

	// Name returns the name of the transport for logging and debugging purposes.
	Name() string
}
```

You can then use this as follows:

```go
k := kindling.NewKindling(
	"myapp",
    kindling.WithTransport(myCoolTransport),
	kindling.WithTransport(myCoolerTransport),
)
httpClient := k.NewHTTPClient()
```

## I want to add fuel to the fire (aka a new bootrapping technique!). What do I do?
All you really need to do is to return an `http.RoundTripper` from whatever library you're adding. Then you simply need to add a method in `kindling.go` to allow callers to configure the new method. For DNS tunneling, for example, that method is as follows:

```
func WithDNSTunnel(d dnstt.DNSTT) Option {
	return newOption(func(k *kindling) {
		log.Info("Setting DNS tunnel")
		if d == nil {
			log.Error("DNSTT instance is nil")
			return
		}
		k.roundTripperGenerators = append(k.roundTripperGenerators, namedDialer("dnstt", d.NewRoundTripper))
	})
}
```

It is also important to document any steps that kindling users must take in order to make the technique operational, if any. Does it require server-side components, for example?

Otherwise, just open a pull request, and we'll take it for a spin and will integrate it as soon as possible.
