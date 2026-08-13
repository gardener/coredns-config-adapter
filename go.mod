module github.com/gardener/coredns-config-adapter

// Note this minimum version requirement. CoreDNS supports the last two
// Go versions. This follows the upstream Go project support.
go 1.26.1

require (
	github.com/coredns/caddy v1.1.4
	github.com/fsnotify/fsnotify v1.10.1
	github.com/golang/mock v1.6.0
	golang.org/x/tools v0.49.0
)

require (
	golang.org/x/mod v0.39.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260811182544-a038080d80e5 // indirect
)
