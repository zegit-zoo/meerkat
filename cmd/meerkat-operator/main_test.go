package main

import (
	"errors"
	"flag"
	"io"
	"slices"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
)

func TestParseFlags_Defaults(t *testing.T) {
	o, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := options{metricsAddr: "0", probeAddr: ":8081", webhooks: true, webhookPort: 9443,
		serverImage: "ghcr.io/zegit-zoo/meerkat:latest", allowedImages: "ghcr.io/zegit-zoo/meerkat"}
	if o != want {
		t.Errorf("defaults = %+v, want %+v", o, want)
	}
}

func TestParseFlags_Set(t *testing.T) {
	o, err := parseFlags([]string{"--leader-elect", "--webhooks=false", "--webhook-port=10250", "--metrics-bind-address=:8080", "--webhook-cert-dir=/certs",
		"--server-image=ghcr.io/zegit-zoo/meerkat:0.21.0", "--allowed-images=", "--watch-namespaces=a,b"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !o.leaderElect || o.webhooks || o.webhookPort != 10250 || o.metricsAddr != ":8080" || o.webhookCertDir != "/certs" ||
		o.serverImage != "ghcr.io/zegit-zoo/meerkat:0.21.0" || o.allowedImages != "" || o.namespaces != "a,b" {
		t.Errorf("parsed = %+v", o)
	}
}

func TestParseFlags_Errors(t *testing.T) {
	if _, err := parseFlags([]string{"--version"}, io.Discard); !errors.Is(err, errVersion) {
		t.Errorf("--version: err = %v, want errVersion", err)
	}
	if _, err := parseFlags([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: err = %v, want flag.ErrHelp", err)
	}
	if _, err := parseFlags([]string{"--nope"}, io.Discard); err == nil {
		t.Error("an unknown flag was accepted")
	}
	if _, err := parseFlags([]string{"extra"}, io.Discard); err == nil {
		t.Error("a positional argument was accepted")
	}
}

func TestNewScheme_HasTheOperatorTypes(t *testing.T) {
	s, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"KnowledgeBase", "MeerkatMob"} {
		if !s.Recognizes(v1alpha1.GroupVersion.WithKind(kind)) {
			t.Errorf("scheme does not know %s", kind)
		}
	}
}

func TestDefaultServerImage_FollowsTheVersion(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "v0.22.0"
	if got := defaultServerImage(); got != "ghcr.io/zegit-zoo/meerkat:0.22.0" {
		t.Errorf("defaultServerImage() = %q", got)
	}
}

func TestWatchedNamespaces(t *testing.T) {
	for _, tc := range []struct {
		flag, pod string
		want      []string // nil: every namespace
	}{
		{"", "meerkat-system", []string{"meerkat-system"}},
		{"*", "meerkat-system", nil},
		{" * ", "", nil},
		{"team-a, team-b", "meerkat-system", []string{"team-a", "team-b"}},
		{"team-a,team-a", "", []string{"team-a"}},
	} {
		got, err := watchedNamespaces(tc.flag, tc.pod)
		if err != nil {
			t.Errorf("watchedNamespaces(%q, %q): %v", tc.flag, tc.pod, err)
			continue
		}
		if tc.want == nil {
			if got != nil {
				t.Errorf("watchedNamespaces(%q, %q) = %v, want every namespace", tc.flag, tc.pod, got)
			}
			continue
		}
		if names := namespaceNames(got); !slices.Equal(names, tc.want) {
			t.Errorf("watchedNamespaces(%q, %q) = %v, want %v", tc.flag, tc.pod, names, tc.want)
		}
	}
	for _, tc := range []struct{ flag, pod string }{
		{"", ""},         // no flag and no POD_NAMESPACE
		{"*,team-a", ""}, // the wildcard is all or nothing
		{"Team-A", ""},   // not a namespace name
		{"team-a,", ""},  // an empty entry
	} {
		if got, err := watchedNamespaces(tc.flag, tc.pod); err == nil {
			t.Errorf("watchedNamespaces(%q, %q) = %v, want an error", tc.flag, tc.pod, got)
		}
	}
}
