package refresh

import "github.com/prometheus/client_golang/prometheus"

// metrics is the refresh controller's Prometheus instrumentation.
//
// # Label discipline
//
// The same rule internal/mcp/metrics.go documents applies here, and it
// applies harder: these series describe object storage, so the obvious
// labels are exactly the ones that must not be used.
//
//	NOT a label:  collection name, bucket, object name, prefix, memory
//	              key, principal, generation, fingerprint, error text.
//	IS a label:   the collection's configuration ORDINAL (bounded by the
//	              number of mounted collections) and the target KIND
//	              (a closed set of two).
//
// A bucket or object name would publish a deployment's storage layout on
// an unauthenticated endpoint. A generation or fingerprint would mint a
// brand-new time series on every publication — a cardinality leak that
// grows forever and, on a shared Prometheus, takes other tenants down
// with it. Both belong in the structured status and the log, where they
// are useful and bounded, and that is where Outcome.Version goes.
//
// Mapping an ordinal back to a collection name is a lookup in the
// configuration an operator already has, and a log line already carries
// the name.
type metrics struct {
	attemptsVec    *prometheus.CounterVec
	changesVec     *prometheus.CounterVec
	failuresVec    *prometheus.CounterVec
	skippedVec     *prometheus.CounterVec
	durationVec    *prometheus.HistogramVec
	lastSuccessVec *prometheus.GaugeVec
	degradedVec    *prometheus.GaugeVec
}

// refreshLabels are the two bounded label names every refresh series
// carries. "collection" holds an ORDINAL, not a name — see the type
// comment.
var refreshLabels = []string{"collection", "kind"}

// refreshBuckets bound a refresh cycle's latency. A no-change probe is a
// single metadata round-trip (milliseconds); a full re-resolve of a
// large prefix plus an index rebuild is seconds to a minute. The default
// buckets top out at 10s, which would collapse every real rebuild into
// +Inf, so the range is extended rather than reused.
var refreshBuckets = []float64{0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

func newMetrics(reg *prometheus.Registry) *metrics {
	m := &metrics{
		attemptsVec: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_refresh_attempts_total",
			Help: "Reconciliation cycles started, by collection ordinal and target kind (content, memory).",
		}, refreshLabels),
		changesVec: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_refresh_changes_total",
			Help: "Reconciliation cycles that found a new source version and swapped a new snapshot in.",
		}, refreshLabels),
		failuresVec: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_refresh_failures_total",
			Help: "Reconciliation cycles that failed; the last known-good snapshot kept serving.",
		}, refreshLabels),
		skippedVec: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_refresh_skipped_total",
			Help: "Reconciliation cycles skipped because one was already in flight for the same collection.",
		}, refreshLabels),
		durationVec: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_refresh_duration_seconds",
			Help:    "Reconciliation cycle latency, including the probe and (when the source changed) the re-resolve, rebuild and swap.",
			Buckets: refreshBuckets,
		}, refreshLabels),
		lastSuccessVec: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "meerkat_refresh_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last reconciliation cycle that completed without error (a no-change probe counts).",
		}, refreshLabels),
		degradedVec: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "meerkat_refresh_degraded",
			Help: "1 when the most recent reconciliation cycle failed and the last known-good snapshot is being served, else 0.",
		}, refreshLabels),
	}
	if reg != nil {
		reg.MustRegister(
			m.attemptsVec, m.changesVec, m.failuresVec, m.skippedVec,
			m.durationVec, m.lastSuccessVec, m.degradedVec,
		)
	}
	return m
}

// init publishes a zero for every series of one target, so a target that
// has never failed reports 0 rather than nothing at all.
func (m *metrics) init(k Key) {
	m.attempts(k).Add(0)
	m.changes(k).Add(0)
	m.failures(k).Add(0)
	m.skipped(k).Add(0)
	m.setDegraded(k, false)
}

func (m *metrics) attempts(k Key) prometheus.Counter {
	return m.attemptsVec.WithLabelValues(k.Label(), k.Kind)
}

func (m *metrics) changes(k Key) prometheus.Counter {
	return m.changesVec.WithLabelValues(k.Label(), k.Kind)
}

func (m *metrics) failures(k Key) prometheus.Counter {
	return m.failuresVec.WithLabelValues(k.Label(), k.Kind)
}

func (m *metrics) skipped(k Key) prometheus.Counter {
	return m.skippedVec.WithLabelValues(k.Label(), k.Kind)
}

func (m *metrics) duration(k Key) prometheus.Observer {
	return m.durationVec.WithLabelValues(k.Label(), k.Kind)
}

func (m *metrics) lastSuccess(k Key) prometheus.Gauge {
	return m.lastSuccessVec.WithLabelValues(k.Label(), k.Kind)
}

func (m *metrics) setDegraded(k Key, degraded bool) {
	v := 0.0
	if degraded {
		v = 1.0
	}
	m.degradedVec.WithLabelValues(k.Label(), k.Kind).Set(v)
}
