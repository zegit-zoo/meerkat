package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/api/option"
)

const (
	secretBucket = "acme-secret-bucket"
	secretObject = "private/handbook-v7.tar.gz"
)

// assertNoForbidden fails if any exported span, in any field, carries a
// URL, the bucket or the object name.
func assertNoForbidden(t *testing.T, exp *tracetest.InMemoryExporter, forbidden ...string) {
	t.Helper()
	for _, s := range exp.GetSpans() {
		var all []string
		all = append(all, s.Name, s.Status.Description)
		for _, a := range s.Attributes {
			all = append(all, string(a.Key), a.Value.String())
		}
		for _, ev := range s.Events {
			all = append(all, ev.Name)
			for _, a := range ev.Attributes {
				all = append(all, string(a.Key), a.Value.String())
			}
		}
		for _, v := range all {
			for _, f := range append(forbidden, "http://", "https://", "url.full") {
				if strings.Contains(v, f) {
					t.Fatalf("span %q leaks %q in %q", s.Name, f, v)
				}
			}
		}
	}
}

// TestRealGCSClientSpansCarryNoURLBucketOrObject drives the real storage
// client against a fake endpoint with the global provider installed. The
// SDK's own instrumentation is left ON here, so this proves the
// allowlist processor holds even when a third party emits spans.
func TestRealGCSClientSpansCarryNoURLBucketOrObject(t *testing.T) {
	clearOTelEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"name":%q,"bucket":%q,"generation":"7"}`, secretObject, secretBucket)
	}))
	defer srv.Close()

	exp := tracetest.NewInMemoryExporter()
	tel, err := New(context.Background(), Options{
		Config:       &Config{Traces: TraceConfig{Enabled: true}},
		SpanExporter: exp,
		SetGlobals:   true,
	})
	if err != nil || tel == nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	for name, opts := range map[string][]option.ClientOption{
		"sdk instrumentation on":  {},
		"sdk instrumentation off": {option.WithTelemetryDisabled()},
	} {
		t.Run(name, func(t *testing.T) {
			exp.Reset()
			opts = append(opts, option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication())
			c, err := storage.NewClient(context.Background(), opts...)
			if err != nil {
				t.Fatalf("storage.NewClient: %v", err)
			}
			defer c.Close()
			ctx, root := tel.Start(context.Background(), "meerkat.test")
			if _, err := c.Bucket(secretBucket).Object(secretObject).Attrs(ctx); err != nil {
				t.Fatalf("Attrs: %v", err)
			}
			root.End()
			if err := tel.ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(exp.GetSpans()) == 0 {
				t.Fatal("no spans exported")
			}
			assertNoForbidden(t, exp, secretBucket, secretObject, "handbook", srv.Listener.Addr().String())
		})
	}
}

func TestAllowlistDropsForeignAttributesAndRenamesThirdPartySpans(t *testing.T) {
	clearOTelEnv(t)
	exp := tracetest.NewInMemoryExporter()
	tel := newTestTelemetry(t, exp, nil)

	// A "third party": a tracer from the same provider, other scope.
	ext := tel.tracerProvider.Tracer("example.com/some/instrumentation")
	_, es := ext.Start(context.Background(), "GET https://storage.example/b/x/o/y", trace.WithSpanKind(trace.SpanKindClient))
	es.SetAttributes(attribute.String("url.full", "https://storage.example/b/x/o/y"), attribute.String("http.request.method", "GET"))
	es.AddEvent("fetching b/x/o/y")
	es.RecordError(errors.New("GET https://storage.example/b/x/o/y: 500"))
	es.SetStatus(1, "GET https://storage.example/b/x/o/y: 500")
	es.End()

	_, ms := tel.Start(context.Background(), "meerkat.test")
	ms.SetAttributes(KeyMCPTool.String("mk_search"), attribute.String("page.id", "secret/page"), attribute.String("db.statement", "x"))
	ms.End()
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	byName := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		byName[s.Name] = s
	}
	e, ok := byName["external.client"]
	if !ok {
		t.Fatalf("third-party span not renamed: %v", byName)
	}
	if len(e.Events) != 0 || e.Status.Description != "" {
		t.Fatalf("third-party events/status text survived: %+v", e)
	}
	if len(e.Attributes) != 1 || e.Attributes[0].Key != "http.request.method" {
		t.Fatalf("third-party attributes = %v, want only http.request.method", e.Attributes)
	}
	m := byName["meerkat.test"]
	if len(m.Attributes) != 1 || m.Attributes[0].Key != KeyMCPTool {
		t.Fatalf("meerkat attributes = %v, want only meerkat.mcp.tool", m.Attributes)
	}
}

func TestEndRecordsAClassifiedReasonNotTheErrorText(t *testing.T) {
	clearOTelEnv(t)
	exp := tracetest.NewInMemoryExporter()
	tel := newTestTelemetry(t, exp, nil)

	cases := []struct {
		err  error
		want string
	}{
		{errors.New(`page "customers/acme/secret-plan" not found in index`), OutcomeError},
		{fmt.Errorf("load %q: %w", "page-id-123", fs.ErrNotExist), OutcomeNotFound},
		{fmt.Errorf("wait: %w", context.DeadlineExceeded), OutcomeTimeout},
		{context.Canceled, OutcomeCancelled},
	}
	for _, c := range cases {
		exp.Reset()
		_, span := tel.Start(context.Background(), "meerkat.test")
		End(span, c.err)
		if err := tel.ForceFlush(context.Background()); err != nil {
			t.Fatal(err)
		}
		s := exp.GetSpans()[0]
		if s.Status.Description != c.want {
			t.Errorf("status = %q, want %q", s.Status.Description, c.want)
		}
		assertNoForbidden(t, exp, "secret-plan", "customers", "page-id-123")
		if len(s.Events) != 0 {
			t.Errorf("End recorded %d events", len(s.Events))
		}
	}
	// A nil error just ends the span.
	_, span := tel.Start(context.Background(), "meerkat.ok")
	End(span, nil)
}

func TestOutboundClientSpanRecordsNoURLOnTransportError(t *testing.T) {
	clearOTelEnv(t)
	exp := tracetest.NewInMemoryExporter()
	tel := newTestTelemetry(t, exp, nil)
	c := tel.HTTPClient(&http.Client{})
	// A closed port: the *url.Error embeds the full URL.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/.well-known/openid-configuration"
	srv.Close()
	resp, err := c.Get(url)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected a transport error")
	}
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoForbidden(t, exp, "openid-configuration")
}
