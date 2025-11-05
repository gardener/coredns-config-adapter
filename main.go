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
	"strings"
	"sync"

	"github.com/coredns/caddy/caddyfile"

	"github.com/fsnotify/fsnotify"
)

func main() {
	inputDir := flag.String("inputDir", "/etc/custom", "Path to the input directory containing custom CoreDNS configuration files")
	outputDir := flag.String("outputDir", "/etc/generated-config", "Path to the output directory where to write the CoreDNS config file to")
	bindStatement := flag.String("bind", "bind 169.254.20.10 10.255.128.10", "Bind statement to insert")

	flag.Parse()
	writeMu := &sync.Mutex{}

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
	err = writeNewConfigToFile(*inputDir, *outputDir, *bindStatement)
	writeMu.Unlock()
	if err != nil {
		log.Println("Error writing new config:", err)
	}

	log.Println("Starting watch handler")
	startWatcher(watcher, inputDir, outputDir, bindStatement, writeMu)

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

func writeNewConfigToFile(inputDir, outputDir, bindStatement string) error {
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

			err = buildServerConfig(serverConfig, bindStatement, &buf)
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

func startWatcher(watcher *fsnotify.Watcher, inputDir, outputDir, bindStatement *string, writeMu *sync.Mutex) {
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
					err := writeNewConfigToFile(*inputDir, *outputDir, *bindStatement)
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

func buildServerConfig(serverConfig []byte, bindStatement string, buf *bytes.Buffer) error {
	serverBlocks, err := loadServerBlocks("Corefile", bytes.NewReader(serverConfig))
	if err != nil {
		return fmt.Errorf("error loading server blocks: %w", err)
	}

	var updatedBlocks []caddyfile.ServerBlock
	for _, block := range serverBlocks {
		for i := len(block.Keys) - 1; i >= 0; i-- {
			if strings.Contains(string(block.Keys[i]), ":8053") {
				block.Keys[i] = strings.Replace(string(block.Keys[i]), ":8053", ":53", 1)
				continue
			}

			if strings.HasSuffix(string(block.Keys[i]), ":53") {
				continue
			}
			// Remove this key from the block as it is has no port with number 8053 or 53 specified
			block.Keys = append(block.Keys[:i], block.Keys[i+1:]...)
		}
		if len(block.Keys) > 0 {
			updatedBlocks = append(updatedBlocks, block)
		}
	}

	for _, block := range updatedBlocks {
		buf.WriteString(strings.Join(block.Keys, " ") + " {\n")
		buf.WriteString("    " + bindStatement + "\n")
		for _, token := range block.Tokens {
			// Skip original bind directives if set as a bind statement is always added
			if len(token) > 0 && strings.EqualFold(string(token[0].Text), "bind") {
				continue
			}

			texts := func(tokens []caddyfile.Token) string {
				if len(tokens) == 0 {
					return ""
				}

				var result strings.Builder
				indentLevel := 0
				for i, t := range tokens {
					text := string(t.Text)
					if i > 0 && t.Line != tokens[i-1].Line {
						result.WriteString("\n")
						currentIndent := indentLevel
						if text == "}" && currentIndent > 0 {
							currentIndent--
						}

						if currentIndent > 0 {
							result.WriteString(strings.Repeat("    ", currentIndent))
						}
					} else if i > 0 {
						result.WriteString(" ")
					}

					result.WriteString(text)
					if text == "{" {
						indentLevel++
					} else if text == "}" && indentLevel > 0 {
						indentLevel--
					}
				}
				return result.String()
			}(token)

			lines := strings.Split(texts, "\n")
			for _, line := range lines {
				buf.WriteString("    " + line + "\n")
			}
		}
		buf.WriteString("}\n\n")
	}
	return nil
}
