module github.com/gardener/coredns-config-adapter

// Note this minimum version requirement. CoreDNS supports the last two
// Go versions. This follows the upstream Go project support.
go 1.25.0

require (
	github.com/coredns/caddy v1.1.4
	github.com/fsnotify/fsnotify v1.9.0
	github.com/golang/mock v1.6.0
	golang.org/x/tools v0.41.0
)

require (
	golang.org/x/mod v0.32.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/telemetry v0.0.0-20260109210033-bd525da824e2 // indirect
)
