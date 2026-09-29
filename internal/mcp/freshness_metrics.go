package mcp

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/zegit-zoo/meerkat/internal/collections"
)

// freshnessCollector publishes meerkat_collection_freshness{state}: how
// many collections are in each freshness state (meerkat-mob#25 part C).
//
// It is computed at scrape time from the collections' own records, so it
// cannot drift from what mk_list_collections reports. The only label is
// the state, a closed set of six, so the series are bounded. No
// collection name, path, commit or token is ever a label (MK-FRESH-10).
//
// Only collections that HAVE a record are counted: a collection without
// a `type: local` refresh: block is absent, not "unknown". When no
// collection has a record the collector emits nothing at all, so an
// unconfigured server's /metrics is byte-for-byte what it was.
type freshnessCollector struct {
	reg  *collections.Registry
	desc *prometheus.Desc
}

func newFreshnessCollector(reg *collections.Registry) *freshnessCollector {
	return &freshnessCollector{
		reg: reg,
		desc: prometheus.NewDesc("meerkat_collection_freshness",
			"Collections in each freshness state (current, behind-disk, behind-remote, dirty, diverged, unknown). "+
				"Only type: local collections with a refresh: block are counted.",
			[]string{"state"}, nil),
	}
}

func (c *freshnessCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *freshnessCollector) Collect(ch chan<- prometheus.Metric) {
	counts := make(map[string]int)
	any := false
	for _, col := range c.reg.All() {
		if f, ok := col.Freshness(); ok {
			any = true
			counts[f.State]++
		}
	}
	if !any {
		return
	}
	for _, state := range collections.FreshnessStates() {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts[state]), state)
	}
}
