package sqlite

import (
	"math"
	"strings"
	"testing"

	"ragfy/internal/knowledge"
)

func TestQueryTermsSupportsMultilingualTextAndDeduplicates(t *testing.T) {
	terms := queryTerms("Como o serviço persistente evita recarregamento? Serviço multilíngue.")
	joined := strings.Join(terms, "|")
	for _, expected := range []string{"como", "serviço", "persistente", "evita", "recarregamento", "multilíngue"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing %q in terms %#v", expected, terms)
		}
	}
	count := 0
	for _, term := range terms {
		if term == "serviço" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("serviço appears %d times in %#v", count, terms)
	}
}

func TestVectorAndRankingHelpers(t *testing.T) {
	if actual := cosine([]float32{1, 0}, []float32{1, 0}); math.Abs(actual-1) > 1e-9 {
		t.Fatalf("cosine identical = %f", actual)
	}
	if actual := cosine([]float32{1}, []float32{1, 0}); actual != 0 {
		t.Fatalf("cosine mismatched dimensions = %f", actual)
	}
	if actual := lexicalScore([]string{"daemon", "sqlite"}, "The daemon is persistent"); actual != 0.5 {
		t.Fatalf("lexical score = %f", actual)
	}
	hits := []knowledge.Hit{{Score: 0.2}, {Score: 0.9}, {Score: 0.5}}
	sortHits(hits)
	if hits[0].Score != 0.9 || hits[2].Score != 0.2 {
		t.Fatalf("hits not sorted: %#v", hits)
	}
	vector := []float32{3.25}
	if actual := decodeVector(encodeVector(vector)); len(actual) != 1 || actual[0] != 3.25 {
		t.Fatalf("vector round trip = %#v", actual)
	}
}
