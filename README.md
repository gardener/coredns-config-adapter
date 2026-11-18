# coredns-config-adapter

This project provides a dynamic adapter that makes custom CoreDNS server blocks effective when `node-local-dns` is used. It watches a directory for `.server` files (such as those mounted from the `coredns-custom` ConfigMap), extracts the zones of their server blocks, and generates a `node-local-dns` configuration file that forwards queries for these zones to CoreDNS.

Without it, `node-local-dns` would send queries for custom zones to the node's upstream resolver via its catch-all (`.`) server block, bypassing CoreDNS and therefore the custom configuration.

## Features

- **Automatic Directory Watching:** Monitors a directory for creation, modification, and deletion of `.server` files.
- **Kubernetes-Ready:** Handles atomic symlink swaps as performed by Kubernetes ConfigMap and Secret mounts.
- **Forward-Only Server Blocks:** Generates one fixed server block per custom zone that forwards to CoreDNS. The bodies of the custom server blocks are never copied, so CoreDNS remains the only place where they are applied.
- **Custom Bind Statement:** Inserts a configurable `bind` statement into each server block.
- **Automatic Regeneration:** Rewrites the output configuration file whenever a relevant change is detected.

## Usage

### Build

```sh
go build -o coredns-config-adapter
```

### Run

```sh
./coredns-config-adapter \
  -inputDir /etc/custom \
  -outputDir /etc/generated-config \
  -bind "bind 169.254.20.10 10.255.128.10" \
  -forceTCP=true
```

- `-inputDir`: Directory containing `.server` files (default: `/etc/custom`)
- `-outputDir`: Directory to write the generated config file (default: `/etc/generated-config`)
- `-bind`: Bind statement to insert into each server block
- `-forceTCP`: Force TCP when forwarding to CoreDNS (default: `true`). Set `-forceTCP=false` to generate `prefer_udp`, which prefers UDP even for incoming TCP queries and retries over TCP if the response is truncated. Gardener should pass the effective Shoot `forceTCPToClusterDNS` setting through this flag.
- `-skipZones`: Comma-separated list of zones for which no server block is generated, including their subzones (default: `cluster.local,in-addr.arpa,ip6.arpa`). These zones are already forwarded to CoreDNS by the `node-local-dns` base configuration, and an exact duplicate would prevent `node-local-dns` from loading its configuration. Set this if the cluster domain differs from `cluster.local`.

For every server block key that is a plain DNS zone with port `8053` (CoreDNS) or `53`, the adapter emits:

```corefile
<zone>:53 {
    <bind statement>
    errors
    prometheus :9253
    cache {
        success 9984 30
        denial 9984 5
    }
    forward . <upstream> {
        force_tcp
    }
}
```

The cache policy matches the `node-local-dns` base configuration for cluster DNS:
successful responses have a maximum cache TTL of 30 seconds, and negative
responses have a maximum cache TTL of 5 seconds, with capacity 9984 per cache
type and per generated zone. Cache memory usage therefore grows with the number
of custom zones. By default, forwarding to CoreDNS uses TCP even when the client
queries over UDP; `-forceTCP=false` uses `prefer_udp` instead. Gardener already
supports TCP DNS to CoreDNS. Other deployments must also allow TCP DNS, including
for fallback after truncated UDP responses. This does not change how CoreDNS
forwards queries to custom upstream resolvers.

Each generated block enables `prometheus :9253`, sharing the metrics endpoint
with the base configuration. Custom-zone DNS requests and cache hits are thereby
included in metrics such as `coredns_dns_requests_total` and
`coredns_cache_hits_total`. If needed, adjust metric selectors that filter by
zone to include the custom zones.

Zone names are normalized (lower case, without trailing dot) and de-duplicated across all files. Keys that are not plain zone names on port `53` or `8053` (e.g. other ports, other transports such as `tls://`, CIDR notation, or the root zone `.`) are ignored and logged.

The `<upstream>` address is read from the `KUBE_DNS_UPSTREAM_SERVICE_HOST`
environment variable, which the kubelet injects into every Pod for the
`kube-dns-upstream` Service that node-local-dns creates via its
`-upstreamsvc` argument. Operators do not need to configure the address
manually; the binary fails fast on startup if the env var is missing.

### Example

Suppose you have a Kubernetes ConfigMap mounted at `/etc/custom` with a file `corp.server`:

```
corp.example:8053 {
    errors
    cache 30
    forward . 10.1.2.3
}
```

The adapter watches this directory and writes `/etc/generated-config/custom-server-block.server`:

```corefile
corp.example:53 {
    bind 169.254.20.10 10.255.128.10
    errors
    prometheus :9253
    cache {
        success 9984 30
        denial 9984 5
    }
    forward . <kube-dns-upstream address> {
        force_tcp
    }
}
```

`node-local-dns` forwards queries for `corp.example` to CoreDNS, which applies the custom server block and forwards them to `10.1.2.3`.

## How It Works

1. **Startup:**  
   - Watches the specified input directory for file changes.
   - Performs an initial config generation.

2. **On Change:**  
   - When a `.server` file is created, modified, or deleted, the adapter:
     - Reads all `.server` files in the directory.
     - Parses each file and collects the zones of its server blocks.
     - Writes one forwarding server block per zone, including the custom bind statement, to the output file.
   - If any `.server` file cannot be parsed, the output file is left unchanged.

3. **Kubernetes Compatibility:**  
   - Handles symlink swaps and atomic updates typical of Kubernetes ConfigMap/Secret mounts.

## Notes

- Only files ending with `.server` are processed. `.override` files are not handled by the adapter.
- The output file is always named `custom-server-block.server` in the output directory.
- The `coredns-conig-adapter` is designed to run as a sidecar or helper in Kubernetes environments.