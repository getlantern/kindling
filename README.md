# Kindling
Library using a series of techniques to send and receive small amounts of data through censoring firewalls.

## Example

```
	kindling := kindling.NewKindling(
		kindling.WithDomainFronting(nil, nil),
		kindling.WithProxyless("example.com"),
		kindling.WithDoHTunnel(),
	)
	httpClient := kindling.NewHTTPClient()
```
