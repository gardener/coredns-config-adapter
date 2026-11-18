// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	testBind       = "bind 169.254.20.10 100.104.0.10"
	testClusterDNS = "100.104.0.11"
)

var testSkipZones = parseSkipZones(defaultSkipZones)

func runBuild(t *testing.T, inputs ...string) string {
	t.Helper()
	zones := map[string]struct{}{}
	for _, input := range inputs {
		if err := collectZones([]byte(input), testSkipZones, zones); err != nil {
			t.Fatalf("collectZones returned error: %v", err)
		}
	}
	var buf bytes.Buffer
	renderServerBlocks(zones, testBind, testClusterDNS, true, &buf)
	return buf.String()
}

// assertForwardOnly re-parses the generated configuration and verifies that it
// consists exclusively of the fixed forwarding template, one block per
// expected zone, and nothing else.
func assertForwardOnly(t *testing.T, out string, wantZones ...string) {
	t.Helper()
	assertForwardOnlyWithTransport(t, out, true, wantZones...)
}

func assertForwardOnlyWithTransport(t *testing.T, out string, forceTCP bool, wantZones ...string) {
	t.Helper()
	blocks, err := loadServerBlocks("Corefile", strings.NewReader(out))
	if err != nil {
		t.Fatalf("generated config does not parse: %v\n%s", err, out)
	}

	var gotZones []string
	for _, block := range blocks {
		if len(block.Keys) != 1 {
			t.Fatalf("expected exactly one key per block, got %q\n%s", block.Keys, out)
		}
		gotZones = append(gotZones, block.Keys[0])

		got := map[string]string{}
		for directive, tokens := range block.Tokens {
			var texts []string
			for _, tok := range tokens {
				texts = append(texts, tok.Text)
			}
			got[directive] = strings.Join(texts, " ")
		}
		transport := "prefer_udp"
		if forceTCP {
			transport = "force_tcp"
		}
		want := map[string]string{
			"bind":       testBind,
			"errors":     "errors",
			"prometheus": "prometheus :9253",
			"cache":      "cache { success 9984 30 denial 9984 5 }",
			"forward":    "forward . " + testClusterDNS + " { " + transport + " }",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("block %q has unexpected directives:\n got: %v\nwant: %v\n%s", block.Keys[0], got, want, out)
		}
	}

	want := make([]string, 0, len(wantZones))
	for _, z := range wantZones {
		want = append(want, z+":53")
	}
	sort.Strings(want)
	if !reflect.DeepEqual(gotZones, want) {
		t.Errorf("unexpected zones:\n got: %q\nwant: %q\n%s", gotZones, want, out)
	}
}

func TestExactOutput(t *testing.T) {
	out := runBuild(t, `corp.example:8053 {
    errors
    cache {
        success 1024 600
        denial 1024 120
    }
    forward . 10.1.2.3 {
        prefer_udp
    }
}
`)
	want := `corp.example:53 {
    bind 169.254.20.10 100.104.0.10
    errors
    prometheus :9253
    cache {
        success 9984 30
        denial 9984 5
    }
    forward . 100.104.0.11 {
        force_tcp
    }
}

`
	if out != want {
		t.Errorf("unexpected output:\n got: %q\nwant: %q", out, want)
	}
}

func TestForwardTransport(t *testing.T) {
	for _, test := range []struct {
		name     string
		forceTCP bool
	}{
		{name: "forceTCP", forceTCP: true},
		{name: "preferUDP", forceTCP: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			zones := map[string]struct{}{"corp.example": {}, "other.example": {}}
			var buf bytes.Buffer
			renderServerBlocks(zones, testBind, testClusterDNS, test.forceTCP, &buf)
			assertForwardOnlyWithTransport(t, buf.String(), test.forceTCP, "corp.example", "other.example")
		})
	}
}

func TestMetricsEnabledForEveryZone(t *testing.T) {
	out := runBuild(t, "corp.example:8053 other.example:8053 {\n    errors\n}\n")
	if got := strings.Count(out, "prometheus :9253"); got != 2 {
		t.Errorf("expected metrics on port 9253 in both server blocks, got %d:\n%s", got, out)
	}
	assertForwardOnly(t, out, "corp.example", "other.example")
}

func TestEmptyInputProducesEmptyOutput(t *testing.T) {
	if out := runBuild(t, "", "# only a comment\n"); out != "" {
		t.Errorf("expected empty output, got:\n%s", out)
	}
}

// TestBlockBodiesAreNeverCopied ensures that only the zone of a server block is
// used. Plugins (supported by node-local-dns or not), bind statements and
// arbitrary arguments must never reach the generated configuration.
func TestBlockBodiesAreNeverCopied(t *testing.T) {
	out := runBuild(t, `example.com:8053 {
    bind 0.0.0.0
    log
    file /etc/coredns/example.com.zone
    hosts {
        6.6.6.6 example.com
        fallthrough
    }
    template IN ANY example.com {
        answer "{{ .Name }} 60 IN A 10.250.0.5"
    }
    rewrite name exact foo.example.com kubernetes.default.svc.cluster.local
}
`)
	for _, unwanted := range []string{"0.0.0.0", "log", "file", "hosts", "6.6.6.6", "template", "answer", "rewrite"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output must not contain %q:\n%s", unwanted, out)
		}
	}
	assertForwardOnly(t, out, "example.com")
}

func TestKeyPortHandling(t *testing.T) {
	out := runBuild(t, `a.example:8053 {
    errors
}
b.example:53 {
    errors
}
c.example:5353 {
    errors
}
d.example {
    errors
}
`)
	assertForwardOnly(t, out, "a.example", "b.example")
}

func TestKeyNormalizationAndDeduplication(t *testing.T) {
	out := runBuild(t,
		`corp.example:8053 Other.Example.:53 dns://third.example:8053 {
    errors
}
CORP.example.:53 {
    errors
}
`,
		`corp.example:8053 {
    errors
}
`)
	assertForwardOnly(t, out, "corp.example", "other.example", "third.example")
}

func TestInvalidKeysAreIgnored(t *testing.T) {
	longLabel := strings.Repeat("a", 64)
	longName := strings.Repeat(strings.Repeat("a", 63)+".", 4) + "example"
	for _, key := range []string{
		".:8053",
		"tls://secure.example:8053",
		"grpc://secure.example:8053",
		"10.0.0.0/24:8053",
		"*.example:8053",
		"bad..example:8053",
		".example:8053",
		"exa$mple:8053",
		"example.com:53:53",
		longLabel + ".example:8053",
		longName + ":8053",
	} {
		t.Run(key, func(t *testing.T) {
			out := runBuild(t, key+" {\n    errors\n}\n")
			if out != "" {
				t.Errorf("key %q must be ignored, got:\n%s", key, out)
			}
		})
	}
}

func TestSkippedZones(t *testing.T) {
	out := runBuild(t, `cluster.local:8053 svc.cluster.local:8053 foo.svc.cluster.local:53 {
    errors
}
10.in-addr.arpa:8053 in-addr.arpa:53 ip6.arpa:8053 {
    errors
}
notcluster.local:8053 local:8053 {
    errors
}
`)
	assertForwardOnly(t, out, "notcluster.local", "local")
}

func TestCustomSkipZones(t *testing.T) {
	zones := map[string]struct{}{}
	skip := parseSkipZones(" Custom.Domain. ,, in-addr.arpa")
	if want := []string{"custom.domain", "in-addr.arpa"}; !reflect.DeepEqual(skip, want) {
		t.Fatalf("parseSkipZones = %q, want %q", skip, want)
	}
	if err := collectZones([]byte("custom.domain:8053 sub.custom.domain:8053 cluster.local:8053 {\n    errors\n}\n"), skip, zones); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	renderServerBlocks(zones, testBind, testClusterDNS, true, &buf)
	assertForwardOnly(t, buf.String(), "cluster.local")
}

func TestParseErrorIsReturned(t *testing.T) {
	err := collectZones([]byte("broken.example:8053 {\n    errors\n"), testSkipZones, map[string]struct{}{})
	if err == nil {
		t.Fatal("expected an error for unbalanced braces")
	}
}

// TestInjectionViaQuotedKey: a quoted multi-line key must not be emitted as
// raw Corefile structure.
func TestInjectionViaQuotedKey(t *testing.T) {
	out := runBuild(t, `"evil.example:53 {
    bind 0.0.0.0
    whatever_unsupported_plugin
    hosts {
        6.6.6.6 kubernetes.default.svc.cluster.local
    }
}
decoy.invalid:53" {
    errors
}
legit.example:8053 {
    errors
}
`)
	if strings.Contains(out, "0.0.0.0") || strings.Contains(out, "evil") || strings.Contains(out, "decoy") {
		t.Errorf("injected key leaked into output:\n%s", out)
	}
	assertForwardOnly(t, out, "legit.example")
}

// TestInjectionViaMultiLineArgument: tokens following a multi-line quoted
// argument must not turn into standalone directives.
func TestInjectionViaMultiLineArgument(t *testing.T) {
	out := runBuild(t, `x.example:53 {
    log "a
b" bind 0.0.0.0
    rewrite "a
b" unknown_plugin
}
`)
	assertForwardOnly(t, out, "x.example")
}

// TestInjectionViaEscapedQuote: backslash/quote sequences in arguments must not
// desynchronize quoting of the generated configuration.
func TestInjectionViaEscapedQuote(t *testing.T) {
	out := runBuild(t, `y.example:53 {
    log a\" b
    template IN A y.example {
        answer "a\" } z.example:53 { bind 0.0.0.0 } #"
    }
}
`)
	assertForwardOnly(t, out, "y.example")
}

func TestWriteNewConfigToFile(t *testing.T) {
	for _, test := range []struct {
		name     string
		forceTCP bool
	}{
		{name: "forceTCP", forceTCP: true},
		{name: "preferUDP", forceTCP: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			inputDir := t.TempDir()
			outputDir := t.TempDir()
			files := map[string]string{
				"a.server":          "corp.example:8053 {\n    forward . 10.1.2.3\n}\n",
				"b.server":          "corp.example:8053 {\n    errors\n}\nlegacy.example:8053 {\n    file /zones/legacy.db\n}\n",
				"corefile.override": "hosts {\n    6.6.6.6 override.example\n}\n",
				"ignored.txt":       "ignored.example:8053 {\n    errors\n}\n",
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(inputDir, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(inputDir, "dir.server"), 0700); err != nil {
				t.Fatal(err)
			}

			if err := writeNewConfigToFile(inputDir, outputDir, testBind, testClusterDNS, testSkipZones, test.forceTCP); err != nil {
				t.Fatalf("writeNewConfigToFile returned error: %v", err)
			}
			out, err := os.ReadFile(filepath.Join(outputDir, "custom-server-block.server"))
			if err != nil {
				t.Fatal(err)
			}
			assertForwardOnlyWithTransport(t, string(out), test.forceTCP, "corp.example", "legacy.example")
		})
	}
}

func TestWriteNewConfigToFileFailsOnInvalidFile(t *testing.T) {
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(inputDir, "broken.server"), []byte("broken.example:8053 {\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeNewConfigToFile(inputDir, outputDir, testBind, testClusterDNS, testSkipZones, true); err == nil {
		t.Fatal("expected an error for an unparsable server file")
	}
	outputFile := filepath.Join(outputDir, "custom-server-block.server")
	if _, err := os.Stat(outputFile); !os.IsNotExist(err) {
		t.Errorf("output file must not be written when a server file is invalid (stat err: %v)", err)
	}

	previous := runBuild(t, "previous.example:8053 {\n    errors\n}\n")
	if err := os.WriteFile(outputFile, []byte(previous), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeNewConfigToFile(inputDir, outputDir, testBind, testClusterDNS, testSkipZones, false); err == nil {
		t.Fatal("expected an error for an unparsable server file")
	}
	out, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != previous {
		t.Errorf("previous output must remain unchanged on parse errors:\n%s", out)
	}
}
