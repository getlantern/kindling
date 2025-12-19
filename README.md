# Kindling
Library using a series of redundant techniques to send and receive small amounts of data through censoring firewalls. This is ideal for accessing things like configuration files during the bootrapping phase as circumvention tools first start. Kindling is intended to be used by any circumvention tool written in Go that need to reliably fetch configuration data on startup. It is also designed to be easy for any developer to add a new technique that other tools may benefit from.

The techniques integrated include:

1) [Domain fronting](https://en.wikipedia.org/wiki/Domain_fronting).
2) [Proxyless dialing from the Outline SDK](https://github.com/Jigsaw-Code/outline-sdk/tree/main/x/smart) that generally bypasses DNS-based and SNI-based blocking (i.e. works particularly well for broadly used services with a lot of IPs that are not IP-blocked)
3) DNS tunneling via [DNSTT](https://www.bamsoftware.com/software/dnstt/)

The idea is to continually add more techniques as they become available such that all tools have access to the most robust library possible for getting on the network quickly and reliably.

## Example

```go
k := kindling.NewKindling(
	"myapp",
    kindling.WithDomainFronting("https://raw.githubusercontent.com/getlantern/fronted/refs/heads/main/fronted.yaml.gz"),
    kindling.WithProxyless("raw.githubusercontent.com"),
    kindling.WithDNSTunnel(newDNSTT()),
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
