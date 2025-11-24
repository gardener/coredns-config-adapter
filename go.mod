module github.com/gardener/coredns-config-adapter

// Note this minimum version requirement. CoreDNS supports the last two
// Go versions. This follows the upstream Go project support.
go 1.25.0

require (
	github.com/coredns/caddy v1.1.3
	github.com/fsnotify/fsnotify v1.9.0
	github.com/golang/mock v1.6.0
	golang.org/x/tools v0.39.0
)

require (
	golang.org/x/mod v0.30.0 // indirect
	golang.org/x/sync v0.18.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/telemetry v0.0.0-20251111182119-bc8e575c7b54 // indirect
)
