package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/prometheus/client_golang/prometheus"
)

// DefaultJWKSMinRefresh is the shortest interval between two fetches of
// one provider's JWKS. Real key rotations are rare and announced ahead
// by publishing the new key beside the old one, so one fetch every 30 s
// is more than enough to pick one up; it also bounds what a caller who
// can mint tokens with arbitrary key IDs can make this server ask the
// identity provider for.
const DefaultJWKSMinRefresh = 30 * time.Second

// maxJWKSBytes caps a JWKS response. A real key set is a few KiB.
const maxJWKSBytes = 1 << 20

// jwtAlgs are the signature algorithms the key set will parse. The
// verifier built around it enforces which of them a token may actually
// use (RS256 unless configured otherwise); this list only has to be a
// superset.
var jwtAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.EdDSA,
}

// keySetMetrics counts JWKS fetches. The series carry no issuer, key ID
// or token detail: /metrics is unauthenticated.
type keySetMetrics struct {
	fetches   *prometheus.CounterVec
	throttled prometheus.Counter
}

func newKeySetMetrics(reg prometheus.Registerer) *keySetMetrics {
	m := &keySetMetrics{
		fetches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_oidc_jwks_fetches_total",
			Help: "JWKS fetches from an identity provider, by reason (initial, unknown_kid). unknown_kid is a forced refetch.",
		}, []string{"reason"}),
		throttled: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "meerkat_oidc_jwks_refetch_throttled_total",
			Help: "Token verifications that wanted a JWKS refetch (unknown key ID) but were held back by the minimum refresh interval.",
		}),
	}
	m.fetches.WithLabelValues("initial")
	m.fetches.WithLabelValues("unknown_kid")
	if reg != nil {
		for _, c := range []prometheus.Collector{m.fetches, m.throttled} {
			if err := reg.Register(c); err != nil {
				var are prometheus.AlreadyRegisteredError
				if !errors.As(err, &are) {
					continue // metrics must never stop authentication from starting
				}
			}
		}
	}
	return m
}

// jwksKeySet is an oidc.KeySet over a provider's jwks_uri that, unlike
// go-oidc's RemoteKeySet, never refetches because a signature failed.
//
// RemoteKeySet re-reads the remote set whenever a cached key does not
// verify a token, whatever the token's key ID, so any caller who can
// build a token with the right (public) issuer and audience and a future
// expiry can make the server call the identity provider on every
// request. Here a signature that fails against a key the cache already
// holds is simply invalid, and a refetch happens only for a key ID the
// cache has never seen, at most once per minRefresh.
type jwksKeySet struct {
	url        string
	client     *http.Client
	minRefresh time.Duration
	now        func() time.Time
	metrics    *keySetMetrics

	mu   sync.RWMutex
	keys []jose.JSONWebKey

	// fetchMu serialises fetches and guards fetched/lastFetch, so a burst
	// of requests with an unknown key ID costs one outbound call.
	fetchMu   sync.Mutex
	fetched   bool
	lastFetch time.Time
}

var _ oidc.KeySet = (*jwksKeySet)(nil)

func newJWKSKeySet(url string, client *http.Client, minRefresh time.Duration, now func() time.Time, m *keySetMetrics) *jwksKeySet {
	if minRefresh <= 0 {
		minRefresh = DefaultJWKSMinRefresh
	}
	if now == nil {
		now = time.Now
	}
	return &jwksKeySet{url: url, client: client, minRefresh: minRefresh, now: now, metrics: m}
}

// VerifySignature implements oidc.KeySet.
func (k *jwksKeySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	jws, err := jose.ParseSigned(jwt, jwtAlgs)
	if err != nil {
		return nil, fmt.Errorf("malformed jwt: %w", err)
	}
	kid := ""
	if len(jws.Signatures) > 0 {
		kid = jws.Signatures[0].Header.KeyID
	}
	if payload, found, err := verifyWithKeys(jws, k.snapshot(), kid); found {
		return payload, err
	}
	// No cached key carries this key ID: the one case that justifies
	// asking the provider again.
	keys, err := k.refetch(ctx)
	if err != nil {
		return nil, err
	}
	payload, found, err := verifyWithKeys(jws, keys, kid)
	if !found {
		return nil, errors.New("no signing key matches the token's key ID")
	}
	return payload, err
}

func (k *jwksKeySet) snapshot() []jose.JSONWebKey {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.keys
}

// verifyWithKeys tries every key whose ID matches kid (every key when the
// token names none). found reports whether any key was a candidate: when
// it is false the caller may refetch, when it is true a failure is final.
func verifyWithKeys(jws *jose.JSONWebSignature, keys []jose.JSONWebKey, kid string) (payload []byte, found bool, err error) {
	for i := range keys {
		if kid != "" && keys[i].KeyID != kid {
			continue
		}
		found = true
		if p, verr := jws.Verify(&keys[i]); verr == nil {
			return p, true, nil
		}
	}
	if found {
		return nil, true, errors.New("failed to verify token signature")
	}
	return nil, false, nil
}

// refetch returns the current key set, fetching it from the provider
// first unless a fetch already happened within the minimum interval.
func (k *jwksKeySet) refetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	k.fetchMu.Lock()
	defer k.fetchMu.Unlock()

	now := k.now()
	if k.fetched && now.Sub(k.lastFetch) < k.minRefresh {
		// Either this request lost a race with the fetch that just
		// refreshed the set (it will find its key now), or the key ID is
		// unknown and the provider was asked recently.
		k.metrics.throttled.Inc()
		return k.snapshot(), nil
	}
	reason := "unknown_kid"
	if !k.fetched {
		reason = "initial"
	}
	k.fetched, k.lastFetch = true, now // a failed fetch is throttled too
	k.metrics.fetches.WithLabelValues(reason).Inc()

	// A request that gives up must not abort the fetch the others wait on
	// (the client's own timeout bounds it).
	keys, err := k.fetch(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	k.keys = keys
	k.mu.Unlock()
	return keys, nil
}

func (k *jwksKeySet) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, fmt.Errorf("jwks request: %w", err)
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching keys: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return nil, fmt.Errorf("reading keys: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching keys: %s", resp.Status)
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("decoding keys: %w", err)
	}
	var keys []jose.JSONWebKey
	for _, raw := range set.Keys {
		var meta struct {
			Alg string `json:"alg"`
		}
		if json.Unmarshal(raw, &meta) != nil {
			continue
		}
		if meta.Alg != "" && !supportedAlg(meta.Alg) {
			continue // e.g. ES256K: ignore the key, keep the set
		}
		var jwk jose.JSONWebKey
		if err := json.Unmarshal(raw, &jwk); err != nil {
			continue // a key type this library cannot use
		}
		keys = append(keys, jwk)
	}
	return keys, nil
}

func supportedAlg(alg string) bool {
	for _, a := range jwtAlgs {
		if string(a) == alg {
			return true
		}
	}
	return false
}
