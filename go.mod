module github.com/gardener/coredns-config-adapter

// Note this minimum version requirement. CoreDNS supports the last two
// Go versions. This follows the upstream Go project support.
go 1.26.1

require (
	github.com/coredns/caddy/v2 v2.1.1
	github.com/fsnotify/fsnotify v1.10.1
	github.com/golang/mock v1.6.0
	golang.org/x/tools v0.46.0
)

require (
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/telemetry v0.0.0-20260610154732-fb80ec83bdd9 // indirect
)
