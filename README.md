# Kindling
Website: **[kindling.lantern.io](https://kindling.lantern.io)**. Kindling is built by the [Lantern](https://lantern.io) team, and its DNS-tunnel transport is [Soar](https://soar.lantern.io).

Library using a series of redundant techniques to send and receive small amounts of data through censoring firewalls. This is ideal for accessing things like configuration files during the bootrapping phase as circumvention tools first start. Kindling is intended to be used by any circumvention tool written in Go that need to reliably fetch configuration data on startup. It is also designed to be easy for any developer to add a new technique that other tools may benefit from.

The techniques integrated include:

1) [Domain fronting](https://en.wikipedia.org/wiki/Domain_fronting).
2) [Proxyless dialing from the Outline SDK](https://github.com/Jigsaw-Code/outline-sdk/tree/main/x/smart) that generally bypasses DNS-based and SNI-based blocking (i.e. works particularly well for broadly used services with a lot of IPs that are not IP-blocked)
3) DNS tunneling via [Soar](https://github.com/getlantern/soar)
4) AMP caching also via David Fifield with a [Lantern implementation](https://github.com/getlantern/amp).

The idea is to continually add more techniques as they become available such that all tools have access to the most robust library possible for getting on the network quickly and reliably.

## Transport racing and priorities

Kindling races the configured transports against each other and returns the first usable response. Transports race in priority tiers: every transport in the default tier connects in parallel, and a lower-priority tier is dialed only once every transport in the higher-priority tiers has failed to produce a usable response.

DNS tunneling (`WithDNSTunnel`) is registered as a **last resort**. It keeps working under heavy censorship but is slow and low-throughput, so it is only dialed when the faster transports (domain fronting, proxyless dialing, AMP caching) are all blocked. Custom transports added via `WithTransport` default to the top tier; a transport can opt into a later tier by implementing `Priority() int` (higher numbers race later).

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
    kindling.WithDNSTunnel(soarClient),
    kindling.WithAMPCache(ampClient),
)
httpClient := k.NewHTTPClient()
```

You can also dynamically add transports that provide a simple `Transport` interface:

```go
// Transport provides the basic interface that any transport must implement to be used by Kindling.
type Transport interface {
	// NewRoundTripper creates a pre-connected http.RoundTripper. Kindling parallelizes the
	// connection of the transports, but sends the request itself serially to avoid issues with
	// non-idempotent requests, so the connection should be complete when this returns.
	NewRoundTripper(ctx context.Context, addr string) (http.RoundTripper, error)

	// MaxLength returns the maximum request body size this transport supports. Zero means no limit.
	MaxLength() int

	// IsStreamable reports whether this transport supports streaming responses (text/event-stream).
	IsStreamable() bool

	// Name identifies this transport for logging and debugging.
	Name() string

	// RequestTimeout returns the maximum time a single request may spend on this transport.
	// Zero means kindling picks a default.
	RequestTimeout() time.Duration
}
```

A transport can also implement `Priority() int` to race in a later tier.

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
func WithDNSTunnel(d DNSTunnel) Option {
	return func(k *kindling) error {
		if d == nil {
			return fmt.Errorf("dns tunnel is nil")
		}
		nt := &namedTransport{
			name:         string(TransportDNSTunnel),
			isStreamable: true,
			newRT:        d.NewRoundTripper,
			priority:     priorityLastResort,
		}
		if tt, ok := d.(interface{ RequestTimeout() time.Duration }); ok {
			nt.reqTimeout = tt.RequestTimeout()
		}
		if tt, ok := d.(interface{ MaxLength() int }); ok {
			nt.maxLength = tt.MaxLength()
		}
		k.transports = append(k.transports, nt)
		return nil
	}
}
```

It is also important to document any steps that kindling users must take in order to make the technique operational, if any. Does it require server-side components, for example?

Otherwise, just open a pull request, and we'll take it for a spin and will integrate it as soon as possible.
