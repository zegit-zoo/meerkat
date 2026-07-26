// Command contentsync populates meerkat's embed directories from the content
// source declared in content-source.yaml. It is a build-time tool run by
// `make sync` and the GoReleaser before-hook — not part of the shipped binary.
//
// Usage:
//
//	go run ./internal/contentsync [--check]
//
// --check validates content-source.yaml and resolves the source without
// copying (a CI lint for the config).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
)

func main() {
	check := flag.Bool("check", false, "validate config and resolve without copying")
	flag.Parse()

	repoRoot, err := os.Getwd()
	if err != nil {
		fail(err)
	}

	if *check {
		cfg, err := contentsource.Load(repoRoot)
		if err != nil {
			fail(err)
		}
		fmt.Printf("content-source.yaml OK (type=%s)\n", cfg.Content.Type)
		return
	}

	commit, err := contentsource.Sync(repoRoot, os.Stderr)
	if err != nil {
		fail(err)
	}
	// Print only the commit on stdout so callers can capture it.
	fmt.Println(commit)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "contentsync: "+err.Error())
	os.Exit(1)
}
