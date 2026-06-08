// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and Gardener contributors
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
	"sort"
	"strings"
	"sync"

	"github.com/coredns/caddy/caddyfile"

	"github.com/fsnotify/fsnotify"
)

// supportedPlugins lists the CoreDNS plugins compiled into node-local-dns
// (see https://github.com/kubernetes-sigs/node-local-dns/blob/master/cmd/node-cache/main.go).
// Server blocks that reference any directive outside this set cannot be served
// locally and must be forwarded to coredns instead.
var supportedPlugins = map[string]struct{}{
	"bind":        {},
	"bufsize":     {},
	"cache":       {},
	"debug":       {},
	"dns64":       {},
	"errors":      {},
	"forward":     {},
	"health":      {},
	"hosts":       {},
	"loadbalance": {},
	"log":         {},
	"loop":        {},
	"prometheus":  {}, // metrics plugin is registered as "prometheus"
	"pprof":       {},
	"reload":      {},
	"rewrite":     {},
	"template":    {},
	"timeouts":    {},
	"trace":       {},
	"whoami":      {},
}

// kubeDNSUpstreamHostEnv is the env var the kubelet auto-injects into every
// pod for the in-cluster `kube-dns-upstream` Service (Service name
// "kube-dns-upstream" → env var "KUBE_DNS_UPSTREAM_SERVICE_HOST"). The
// node-local-dns DaemonSet creates that Service via its `-upstreamsvc`
// argument and points it at the cluster's CoreDNS Pods, which is exactly
// where the fallback forward statement should send queries.
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

func main() {
	inputDir := flag.String("inputDir", "/etc/custom", "Path to the input directory containing custom CoreDNS configuration files")
	outputDir := flag.String("outputDir", "/etc/generated-config", "Path to the output directory where to write the CoreDNS config file to")
	bindStatement := flag.String("bind", "bind 169.254.20.10 10.255.128.10", "Bind statement to insert")

	flag.Parse()
	writeMu := &sync.Mutex{}

	clusterDNS := os.Getenv(kubeDNSUpstreamHostEnv)
	if clusterDNS == "" {
		log.Fatalf("%s env var is empty; cannot determine the upstream CoreDNS address for fallback server blocks", kubeDNSUpstreamHostEnv)
	}
	log.Println("Using cluster DNS address:", clusterDNS)

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
	writeMu.Lock()
	log.Println("Writing initial configuration file")
	err = writeNewConfigToFile(*inputDir, *outputDir, *bindStatement, clusterDNS)
	writeMu.Unlock()
	if err != nil {
		log.Println("Error writing new config:", err)
	}

	log.Println("Starting watch handler")
	startWatcher(watcher, inputDir, outputDir, bindStatement, &clusterDNS, writeMu)

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

func writeNewConfigToFile(inputDir, outputDir, bindStatement, clusterDNS string) error {
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		return fmt.Errorf("error reading input directory: %w", err)
	}

	var buf bytes.Buffer
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && isServerFile(name) {
			serverConfig, err := os.ReadFile(inputDir + "/" + name) // #nosec #G304 -- Loaded from ConfigMap
			if err != nil {
				return fmt.Errorf("error reading file %s: %w", name, err)
			}

			err = buildServerConfig(serverConfig, bindStatement, clusterDNS, &buf)
			if err != nil {
				return fmt.Errorf("error building server config for file %s: %w", name, err)
			}
		}
	}

	outputFile := outputDir + "/" + "custom-server-block.server"
	log.Println("Writing configuration to", outputFile)
	err = os.WriteFile(outputFile, buf.Bytes(), 0600)
	if err != nil {
		return fmt.Errorf("error writing output file: %w", err)
	}

	return nil
}

func startWatcher(watcher *fsnotify.Watcher, inputDir, outputDir, bindStatement, clusterDNS *string, writeMu *sync.Mutex) {
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				log.Println("event:", event)

				if event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) || event.Has(fsnotify.Write) {
					writeMu.Lock()
					err := writeNewConfigToFile(*inputDir, *outputDir, *bindStatement, *clusterDNS)
					writeMu.Unlock()
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

// renderTokenText returns text suitable for re-emitting the token in a
// Corefile. The caddyfile lexer strips surrounding quotes from a quoted token,
// so any token whose text contains whitespace, '#', or is empty must be
// re-quoted to be parsed back into a single token.
func renderTokenText(text string) string {
	if text == "" {
		return `""`
	}
	if text == "{" || text == "}" {
		return text
	}
	if strings.ContainsAny(text, " \t\r\n#\"") {
		// Escape embedded double quotes; the caddy lexer only recognises
		// '\"' as an escape sequence inside quoted strings.
		escaped := strings.ReplaceAll(text, `"`, `\"`)
		return `"` + escaped + `"`
	}
	return text
}

// renderTokens converts a slice of tokens (the body of a server block) to its
// indented Corefile representation. The first line is at indentLevel 1 (i.e.
// one indent inside the server block braces).
func renderTokens(tokens []caddyfile.Token) string {
	if len(tokens) == 0 {
		return ""
	}

	var result strings.Builder
	indentLevel := 0
	for i, t := range tokens {
		text := renderTokenText(t.Text)
		if i > 0 && t.Line != tokens[i-1].Line {
			result.WriteString("\n")
			currentIndent := indentLevel
			if t.Text == "}" && currentIndent > 0 {
				currentIndent--
			}

			if currentIndent > 0 {
				result.WriteString(strings.Repeat("    ", currentIndent))
			}
		} else if i > 0 {
			result.WriteString(" ")
		}

		result.WriteString(text)
		if t.Text == "{" {
			indentLevel++
		} else if t.Text == "}" && indentLevel > 0 {
			indentLevel--
		}
	}
	return result.String()
}

func buildServerConfig(serverConfig []byte, bindStatement, clusterDNS string, buf *bytes.Buffer) error {
	serverBlocks, err := loadServerBlocks("Corefile", bytes.NewReader(serverConfig))
	if err != nil {
		return fmt.Errorf("error loading server blocks: %w", err)
	}

	type processedBlock struct {
		keys           []string
		tokens         map[string][]caddyfile.Token
		forwardOnly    bool
		unsupportedDir string
	}

	var updatedBlocks []processedBlock
	for _, block := range serverBlocks {
		for i := len(block.Keys) - 1; i >= 0; i-- {
			if strings.Contains(string(block.Keys[i]), ":8053") {
				block.Keys[i] = strings.Replace(string(block.Keys[i]), ":8053", ":53", 1)
				continue
			}

			if strings.HasSuffix(string(block.Keys[i]), ":53") {
				continue
			}
			// Remove this key from the block as it has no port number 8053 or 53 specified
			block.Keys = append(block.Keys[:i], block.Keys[i+1:]...)
		}
		if len(block.Keys) == 0 {
			continue
		}

		pb := processedBlock{
			keys:   block.Keys,
			tokens: block.Tokens,
		}
		// Detect any directive used in this block that is not part of the
		// plugin set compiled into node-local-dns. block.Tokens is a map
		// keyed by directive name, so we can check the keys directly. We
		// iterate in deterministic (sorted) order so that the directive
		// reported in the warning comment is stable across runs.
		dirNames := make([]string, 0, len(block.Tokens))
		for directive := range block.Tokens {
			dirNames = append(dirNames, directive)
		}
		sort.Strings(dirNames)
		for _, directive := range dirNames {
			if _, ok := supportedPlugins[directive]; !ok {
				pb.forwardOnly = true
				pb.unsupportedDir = directive
				break
			}
		}
		updatedBlocks = append(updatedBlocks, pb)
	}

	for _, block := range updatedBlocks {
		buf.WriteString(strings.Join(block.keys, " ") + " {\n")
		buf.WriteString("    " + bindStatement + "\n")

		if block.forwardOnly {
			// Plugin not available in node-local-dns: forward the entire
			// query stream to coredns instead of trying to apply the
			// configuration locally.
			log.Printf("server block %q uses plugin %q which is not available in node-local-dns; forwarding to coredns", strings.Join(block.keys, " "), block.unsupportedDir)
			fmt.Fprintf(buf, "    # plugin %q not available in node-local-dns; forwarding to coredns\n", block.unsupportedDir)
			buf.WriteString("    forward . " + clusterDNS + "\n")
			buf.WriteString("}\n\n")
			continue
		}

		// Emit directive blocks in their original source order. block.tokens
		// is a map keyed by directive name; ranging it directly would
		// produce non-deterministic output. Sort by the line number of the
		// directive's first token so the output matches the input order.
		dirEmitNames := make([]string, 0, len(block.tokens))
		for name := range block.tokens {
			dirEmitNames = append(dirEmitNames, name)
		}
		sort.Slice(dirEmitNames, func(i, j int) bool {
			ti := block.tokens[dirEmitNames[i]]
			tj := block.tokens[dirEmitNames[j]]
			if len(ti) == 0 {
				return true
			}
			if len(tj) == 0 {
				return false
			}
			return ti[0].Line < tj[0].Line
		})

		for _, name := range dirEmitNames {
			token := block.tokens[name]
			// Skip original bind directives if set as a bind statement is always added
			if len(token) > 0 && strings.EqualFold(string(token[0].Text), "bind") {
				continue
			}

			texts := renderTokens(token)

			lines := strings.Split(texts, "\n")
			for _, line := range lines {
				buf.WriteString("    " + line + "\n")
			}
		}
		buf.WriteString("}\n\n")
	}
	return nil
}
