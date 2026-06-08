// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

const (
	testBind       = "bind 169.254.20.10 100.104.0.10"
	testClusterDNS = "100.104.0.10"
)

func runBuild(t *testing.T, input string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := buildServerConfig([]byte(input), testBind, testClusterDNS, &buf); err != nil {
		t.Fatalf("buildServerConfig returned error: %v", err)
	}
	return buf.String()
}

// TestQuotingPreservedForTemplateAnswer covers the `template` plugin's
// `answer` directive, which uses a quoted multi-token string. The lexer
// strips the quotes when parsing, so the adapter has to re-quote any token
// whose text contains whitespace, otherwise the regenerated Corefile
// produces "unclosed action" errors when parsed again.
func TestQuotingPreservedForTemplateAnswer(t *testing.T) {
	input := `example.com:8053 {
    log
    errors
    template IN ANY example.com {
        match "^foo-[0-9a-z]+\.(example\.com\.)$|^bar-.*\.(example\.com\.)$"
        answer "{{ .Name }} 60 IN A 10.250.0.5"
        fallthrough
    }
}
`
	out := runBuild(t, input)

	if !strings.Contains(out, `answer "{{ .Name }} 60 IN A 10.250.0.5"`) {
		t.Errorf("expected `answer` directive to keep its quoted argument; got:\n%s", out)
	}
	// The match regex contains pipe and special characters but no whitespace,
	// so it should NOT be wrapped in quotes (the original input also did not
	// strictly need quotes for it, but it was provided quoted). Either form
	// is acceptable as long as it parses back to a single token.
	if !strings.Contains(out, "match ") {
		t.Errorf("expected `match` directive in output:\n%s", out)
	}

	// Round-trip: the produced Corefile must parse cleanly again.
	if _, err := loadServerBlocks("Corefile", strings.NewReader(out)); err != nil {
		t.Errorf("regenerated config failed to parse: %v\n%s", err, out)
	}
}

// TestQuotingPreservedAcrossRoundTrip ensures that any token whose text
// contains whitespace is re-emitted as a quoted single token.
func TestQuotingPreservedAcrossRoundTrip(t *testing.T) {
	input := `example.org:8053 {
    template IN ANY example.org {
        answer "{{ .Name }} 60 IN A 1.2.3.4"
    }
}
`
	out := runBuild(t, input)

	parsed, err := loadServerBlocks("Corefile", strings.NewReader(out))
	if err != nil {
		t.Fatalf("regenerated config did not parse: %v\n%s", err, out)
	}
	if len(parsed) != 1 {
		t.Fatalf("expected 1 server block in regenerated output, got %d:\n%s", len(parsed), out)
	}
	templateTokens, ok := parsed[0].Tokens["template"]
	if !ok {
		t.Fatalf("regenerated output is missing the template directive:\n%s", out)
	}
	// Find the `answer` token and verify its argument is a single token with
	// the original whitespace-containing text.
	var answerArg string
	for i, tok := range templateTokens {
		if tok.Text == "answer" && i+1 < len(templateTokens) {
			answerArg = templateTokens[i+1].Text
			break
		}
	}
	want := "{{ .Name }} 60 IN A 1.2.3.4"
	if answerArg != want {
		t.Errorf("answer argument = %q, want %q", answerArg, want)
	}
}

// TestUnsupportedPluginIsForwarded covers usage of the `file` and `ready`
// plugins (and others), none of which are compiled into node-local-dns.
// Such blocks must be replaced with a forward to coredns rather than emitted
// verbatim, otherwise node-local-dns crashes with "no action found for
// directive ... (missing a plugin?)".
func TestUnsupportedPluginIsForwarded(t *testing.T) {
	for _, plugin := range []string{"file", "ready", "kubernetes", "auto"} {
		t.Run(plugin, func(t *testing.T) {
			input := "example.com:8053 {\n    " + plugin + " /etc/coredns/example.com.zone\n    log\n}\n"
			out := runBuild(t, input)

			if !strings.Contains(out, "forward . "+testClusterDNS) {
				t.Errorf("expected fallback forward to clusterDNS for unsupported plugin %q; got:\n%s", plugin, out)
			}
			if strings.Contains(out, plugin+" /etc/coredns") {
				t.Errorf("output should not contain unsupported %q directive after rewrite; got:\n%s", plugin, out)
			}
			if !strings.Contains(out, "    "+testBind+"\n") {
				t.Errorf("expected bind statement in forwarded block; got:\n%s", out)
			}
			if _, err := loadServerBlocks("Corefile", strings.NewReader(out)); err != nil {
				t.Errorf("regenerated config failed to parse: %v\n%s", err, out)
			}
		})
	}
}

// TestSupportedPluginsAreEmittedVerbatim ensures that blocks composed only of
// plugins available in node-local-dns continue to be emitted normally (no
// fallback) - i.e. we did not break the existing behaviour.
func TestSupportedPluginsAreEmittedVerbatim(t *testing.T) {
	input := `db.example.com:8053 {
    debug
    cache 30
    forward . 172.30.255.4
}
`
	out := runBuild(t, input)

	if strings.Contains(out, "forward . "+testClusterDNS) {
		t.Errorf("expected NO fallback for fully supported block; got:\n%s", out)
	}
	if !strings.Contains(out, "forward . 172.30.255.4") {
		t.Errorf("expected original forward directive to be preserved; got:\n%s", out)
	}
	if !strings.Contains(out, "    debug\n") {
		t.Errorf("expected debug directive to be preserved; got:\n%s", out)
	}
	if !strings.Contains(out, "db.example.com:53") {
		t.Errorf("expected port :8053 to be rewritten to :53 in keys; got:\n%s", out)
	}
}

// TestMixedBlocksDecidedIndependently verifies that the unsupported-plugin
// fallback is decided per server block - one bad block must not poison its
// neighbours.
func TestMixedBlocksDecidedIndependently(t *testing.T) {
	input := `good.example:8053 {
    cache 30
    forward . 1.2.3.4
}

bad.example:8053 {
    file /etc/coredns/bad.zone
    log
}
`
	out := runBuild(t, input)

	if !strings.Contains(out, "forward . 1.2.3.4") {
		t.Errorf("expected good block's forward to be preserved; got:\n%s", out)
	}
	if !strings.Contains(out, "forward . "+testClusterDNS) {
		t.Errorf("expected bad block to fall back to clusterDNS forward; got:\n%s", out)
	}
	if strings.Contains(out, "file /etc/coredns/bad.zone") {
		t.Errorf("bad block's file directive must not be emitted; got:\n%s", out)
	}
}

// TestBlockWithoutPort53Or8053IsDropped preserves the existing behaviour of
// dropping keys that do not address ports :53 or :8053.
func TestBlockWithoutPort53Or8053IsDropped(t *testing.T) {
	input := `example.org:9999 {
    cache 30
}
`
	out := runBuild(t, input)
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected empty output for non-:53 / non-:8053 block; got:\n%s", out)
	}
}

// TestRenderTokenTextQuoting unit-tests the quoting helper in isolation.
func TestRenderTokenTextQuoting(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"{", "{"},
		{"}", "}"},
		{"", `""`},
		{"with space", `"with space"`},
		{"{{ .Name }} 60 IN A 1.2.3.4", `"{{ .Name }} 60 IN A 1.2.3.4"`},
		{"hash#in#middle", `"hash#in#middle"`},
		{`has"quote`, `"has\"quote"`},
	}
	for _, tc := range cases {
		got := renderTokenText(tc.in)
		if got != tc.want {
			t.Errorf("renderTokenText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestForwardOnlyBlockSkipsOriginalBind verifies that even if the user's
// custom config supplied a bind directive, we never end up with a duplicate
// bind in a forward-only fallback block.
func TestForwardOnlyBlockSkipsOriginalBind(t *testing.T) {
	input := `example.com:8053 {
    bind 1.2.3.4
    file /etc/coredns/example.com.zone
}
`
	out := runBuild(t, input)
	if strings.Contains(out, "bind 1.2.3.4") {
		t.Errorf("user-supplied bind must not leak into forward-only block; got:\n%s", out)
	}
	bindCount := strings.Count(out, "bind 169.254.20.10 100.104.0.10")
	if bindCount != 1 {
		t.Errorf("expected exactly one synthesised bind, got %d:\n%s", bindCount, out)
	}
}

// TestEndToEndMixedSupportedConfig combines several supported server blocks,
// including one using the `template` plugin with a quoted answer, and checks
// the produced output is parseable, preserves the template's quoted answer,
// keeps the supported blocks intact, and produces no "missing plugin" or
// "unclosed action" failures when re-parsed.
func TestEndToEndMixedSupportedConfig(t *testing.T) {
	input := `db-a.example.com:8053 {
    debug
    cache 30
    forward . 172.30.255.4
}
db-b.example.com:8053 {
    debug
    cache 30
    forward . 172.30.255.4
}
templated.example.com:8053 {
    log
    errors
    template IN ANY example.com {
        match "^foo-[0-9a-z]+\.(example\.com\.)$|^bar-.*\.(example\.com\.)$"
        answer "{{ .Name }} 60 IN A 10.250.0.5"
        fallthrough
    }
}
`
	out := runBuild(t, input)

	// Round-trip parse must succeed. This is the property that was broken in
	// production (the lexer reported "unclosed action").
	parsed, err := loadServerBlocks("Corefile", strings.NewReader(out))
	if err != nil {
		t.Fatalf("regenerated config did not parse: %v\n%s", err, out)
	}
	if len(parsed) != 3 {
		t.Fatalf("expected 3 server blocks, got %d:\n%s", len(parsed), out)
	}
	if !strings.Contains(out, `answer "{{ .Name }} 60 IN A 10.250.0.5"`) {
		t.Errorf("template answer lost its quoting; got:\n%s", out)
	}
	if !strings.Contains(out, "forward . 172.30.255.4") {
		t.Errorf("supported forward directive must be preserved; got:\n%s", out)
	}
	// None of the three blocks in this input use unsupported plugins, so the
	// fallback forward to clusterDNS must NOT appear.
	if strings.Contains(out, "forward . "+testClusterDNS) {
		t.Errorf("did not expect fallback for fully-supported config; got:\n%s", out)
	}
}
