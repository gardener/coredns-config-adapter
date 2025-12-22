module github.com/gardener/coredns-config-adapter

// Note this minimum version requirement. CoreDNS supports the last two
// Go versions. This follows the upstream Go project support.
go 1.25.0

require (
	github.com/coredns/caddy v1.1.3
	github.com/fsnotify/fsnotify v1.9.0
	github.com/golang/mock v1.6.0
	golang.org/x/tools v0.40.0
)

require (
	golang.org/x/mod v0.31.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/telemetry v0.0.0-20251203150158-8fff8a5912fc // indirect
)
