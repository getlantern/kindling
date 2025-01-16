# Kindling
Library using a series of techniques to send and receive small amounts of data through censoring firewalls.

## Example

```go
k := kindling.NewKindling(
    kindling.WithDomainFronting("https://url-with-gzipped-domain-fronting-config"),
    kindling.WithProxyless("example.com"),
    //kindling.WithDoHTunnel(),
    //kindling.WithPushNotifications(),
)
httpClient := k.NewHTTPClient()
```
