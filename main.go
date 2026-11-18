// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/coredns/caddy/caddyfile"

	"github.com/fsnotify/fsnotify"
)

// kubeDNSUpstreamHostEnv is the env var the kubelet auto-injects into every
// pod for the in-cluster `kube-dns-upstream` Service (Service name
// "kube-dns-upstream" → env var "KUBE_DNS_UPSTREAM_SERVICE_HOST"). The
// node-local-dns DaemonSet creates that Service via its `-upstreamsvc`
// argument and points it at the cluster's CoreDNS Pods, which is exactly
// where queries for custom zones have to be forwarded to.
//
// Ordering caveat: the kubelet only injects Service env vars for Services
// that already exist when the Pod starts. In Gardener this is fine because
// the kube-dns-upstream Service ships together with the node-local-dns
// DaemonSet in the same ManagedResource, so it always exists before any
// adapter Pod schedules. If this adapter is ever deployed outside that
// wiring, ensure the kube-dns-upstream Service exists before the Pod is
// created — otherwise this env var will be empty and the binary will fail
// fast at startup.
//
// We cannot use node-local-dns's __PILLAR__CLUSTER__DNS__ placeholder here:
// node-local-dns only substitutes pillar variables in the file passed via
// -conf (the base Corefile); files brought in via the `import` directive
// (which is how this generated config is consumed) are read by CoreDNS
// directly at parse time, so a literal __PILLAR__CLUSTER__DNS__ token
// would reach the forward plugin and fail with
// "not an IP address or file: __PILLAR__CLUSTER__DNS__".
const kubeDNSUpstreamHostEnv = "KUBE_DNS_UPSTREAM_SERVICE_HOST"

// defaultSkipZones are the zones that the node-local-dns base Corefile already
// serves by forwarding to CoreDNS. Server blocks for these zones (or any of
// their subzones) are not needed, and an exact duplicate would prevent
// node-local-dns from loading its configuration.
const defaultSkipZones = "cluster.local,in-addr.arpa,ip6.arpa"

// zoneKeyRegexp matches a server block key consisting of a plain DNS zone name
// (optionally prefixed with dns:// and optionally fully qualified) followed by
// the port CoreDNS (8053) or node-local-dns (53) listens on. Anything else,
// e.g. other ports, other transports, CIDR notation, or keys containing
// whitespace or Corefile syntax, is rejected.
var zoneKeyRegexp = regexp.MustCompile(`^(?:dns://)?((?:[A-Za-z0-9_-]{1,63}\.)*[A-Za-z0-9_-]{1,63})\.?:(?:53|8053)$`)

const maxZoneNameLength = 253

// writeMu serializes writes of the generated config file between the initial
// write at startup and subsequent writes triggered by watcher events.
var writeMu sync.Mutex

func main() {
	inputDir := flag.String("inputDir", "/etc/custom", "Path to the input directory containing custom CoreDNS configuration files")
	outputDir := flag.String("outputDir", "/etc/generated-config", "Path to the output directory where to write the CoreDNS config file to")
	bindStatement := flag.String("bind", "bind 169.254.20.10 10.255.128.10", "Bind statement to insert")
	skipZonesFlag := flag.String("skipZones", defaultSkipZones, "Comma-separated list of zones (including their subzones) for which no server block is generated because node-local-dns already forwards them to CoreDNS")
	forceTCP := flag.Bool("forceTCP", true, "Force TCP when forwarding to CoreDNS; if false, prefer UDP with TCP fallback for truncated responses")

	flag.Parse()

	clusterDNS := os.Getenv(kubeDNSUpstreamHostEnv)
	if clusterDNS == "" {
		log.Fatalf("%s env var is empty; cannot determine the upstream CoreDNS address to forward custom zones to", kubeDNSUpstreamHostEnv)
	}
	log.Println("Using cluster DNS address:", clusterDNS)

	skipZones := parseSkipZones(*skipZonesFlag)
	log.Println("Skipping zones already forwarded to CoreDNS:", strings.Join(skipZones, ", "))

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	defer func() {
		if err := watcher.Close(); err != nil {
			log.Println("Error closing watcher:", err)
		}
	}()

	// Initial config write
	log.Println("Writing initial configuration file")
	err = writeNewConfigToFile(*inputDir, *outputDir, *bindStatement, clusterDNS, skipZones, *forceTCP)
	if err != nil {
		log.Println("Error writing new config:", err)
	}

	log.Println("Starting watch handler")
	startWatcher(watcher, *inputDir, *outputDir, *bindStatement, clusterDNS, skipZones, *forceTCP)

	log.Println("Watch directory", *inputDir)
	// Watch input directory for new/deleted and modified files
	err = watcher.Add(*inputDir)
	if err != nil {
		log.Fatal(err)
	}

	<-make(chan struct{})
}

func loadServerBlocks(filename string, input io.Reader) ([]caddyfile.ServerBlock, error) {
	parsed, err := caddyfile.Parse(filename, input, nil)
	if err != nil {
		return nil, err
	}

	return parsed, nil
}

func writeNewConfigToFile(inputDir, outputDir, bindStatement, clusterDNS string, skipZones []string, forceTCP bool) error {
	writeMu.Lock()
	defer writeMu.Unlock()

	entries, err := os.ReadDir(inputDir)
	if err != nil {
		return fmt.Errorf("error reading input directory: %w", err)
	}

	zones := map[string]struct{}{}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && isServerFile(name) {
			serverConfig, err := os.ReadFile(inputDir + "/" + name) // #nosec #G304 -- Loaded from ConfigMap
			if err != nil {
				return fmt.Errorf("error reading file %s: %w", name, err)
			}

			if err := collectZones(serverConfig, skipZones, zones); err != nil {
				return fmt.Errorf("error collecting zones from file %s: %w", name, err)
			}
		}
	}

	var buf bytes.Buffer
	renderServerBlocks(zones, bindStatement, clusterDNS, forceTCP, &buf)

	outputFile := outputDir + "/" + "custom-server-block.server"
	log.Println("Writing configuration to", outputFile)
	err = os.WriteFile(outputFile, buf.Bytes(), 0600)
	if err != nil {
		return fmt.Errorf("error writing output file: %w", err)
	}

	return nil
}

func startWatcher(watcher *fsnotify.Watcher, inputDir, outputDir, bindStatement, clusterDNS string, skipZones []string, forceTCP bool) {
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				log.Println("event:", event)

				if event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) || event.Has(fsnotify.Write) {
					err := writeNewConfigToFile(inputDir, outputDir, bindStatement, clusterDNS, skipZones, forceTCP)
					if err != nil {
						log.Println("Error writing new config:", err)
					}
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}

				log.Println("error:", err)
			}
		}
	}()
}

func isServerFile(name string) bool {
	return strings.HasSuffix(name, ".server")
}

// parseSkipZones turns the comma-separated -skipZones flag value into a list
// of normalized zone names.
func parseSkipZones(value string) []string {
	var zones []string
	for _, z := range strings.Split(value, ",") {
		if z = normalizeZone(z); z != "" {
			zones = append(zones, z)
		}
	}
	return zones
}

func normalizeZone(zone string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(zone), "."))
}

// zoneFromKey returns the normalized zone of a server block key, or false if
// the key is not a plain DNS zone on port 53 or 8053.
func zoneFromKey(key string) (string, bool) {
	match := zoneKeyRegexp.FindStringSubmatch(key)
	if match == nil {
		return "", false
	}
	zone := normalizeZone(match[1])
	if len(zone) > maxZoneNameLength {
		return "", false
	}
	return zone, true
}

func isSkippedZone(zone string, skipZones []string) bool {
	for _, skip := range skipZones {
		if zone == skip || strings.HasSuffix(zone, "."+skip) {
			return true
		}
	}
	return false
}

// collectZones parses a custom CoreDNS server file and adds the zones of all
// its server blocks to zones. Only the server block keys are evaluated; the
// block bodies are never copied to node-local-dns. CoreDNS applies the full
// custom configuration, node-local-dns merely forwards queries for these zones
// to CoreDNS, as they would otherwise bypass CoreDNS via node-local-dns'
// catch-all server block.
func collectZones(serverConfig []byte, skipZones []string, zones map[string]struct{}) error {
	serverBlocks, err := loadServerBlocks("Corefile", bytes.NewReader(serverConfig))
	if err != nil {
		return fmt.Errorf("error loading server blocks: %w", err)
	}

	for _, block := range serverBlocks {
		for _, key := range block.Keys {
			zone, ok := zoneFromKey(key)
			if !ok {
				log.Printf("ignoring server block key %q: not a DNS zone with port 53 or 8053", key)
				continue
			}
			if isSkippedZone(zone, skipZones) {
				log.Printf("ignoring server block key %q: zone is already forwarded to coredns by node-local-dns", key)
				continue
			}
			zones[zone] = struct{}{}
		}
	}
	return nil
}

// renderServerBlocks writes one forwarding server block per zone, sorted by
// zone name. Apart from the validated zone names, the output consists only of
// operator-provided values (bind statement and CoreDNS address).
func renderServerBlocks(zones map[string]struct{}, bindStatement, clusterDNS string, forceTCP bool, buf *bytes.Buffer) {
	transport := "prefer_udp"
	if forceTCP {
		transport = "force_tcp"
	}

	names := make([]string, 0, len(zones))
	for zone := range zones {
		names = append(names, zone)
	}
	sort.Strings(names)

	for _, zone := range names {
		fmt.Fprintf(buf, `%s:53 {
    %s
    errors
    prometheus :9253
    cache {
        success 9984 30
        denial 9984 5
    }
    forward . %s {
        %s
    }
}

`, zone, bindStatement, clusterDNS, transport)
	}
}
