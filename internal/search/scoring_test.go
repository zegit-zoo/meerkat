package search

import (
	"context"
	"strings"
	"testing"

	"github.com/blevesearch/bleve/v2"
	bsearch "github.com/blevesearch/bleve/v2/search"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// scoring_test.go pins #101: the index meerkat searches scores with BM25.
// Before, it was bleve's upsidedown index, which ignores a mapping's
// ScoringModel and always scores with TF-IDF.

// explanationText flattens an explanation tree into one string.
func explanationText(e *bsearch.Explanation) string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(e.Message)
	b.WriteByte('\n')
	for _, c := range e.Children {
		b.WriteString(explanationText(c))
	}
	return b.String()
}

func TestIndex_ScoresWithBM25(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{
		fixturePage("concepts/quorum", "Quorum", "A quorum is the minimum number of replicas that must agree.", "concepts"),
		fixturePage("concepts/sharding", "Sharding", "Data is split across shards by key hash.", "concepts"),
	})
	req := bleve.NewSearchRequest(bleve.NewMatchQuery("quorum"))
	req.Explain = true
	res, err := idx.bleve.SearchInContext(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits")
	}
	expl := explanationText(res.Hits[0].Expl)
	// BM25's term explanation has a saturation node with k1 and a field
	// norm against the field's AVERAGE length; TF-IDF's has neither.
	if !strings.Contains(expl, "saturation(term:") || !strings.Contains(expl, "avgFieldLength") {
		t.Fatalf("the index does not score with BM25; explanation:\n%s", expl)
	}
}
