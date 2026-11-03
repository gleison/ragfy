package retrieval

import (
	"context"
	"math"
	"strings"
	"testing"

	"ragfy/internal/knowledge"
)

type staticCorpus struct {
	documents []knowledge.Document
}

func (corpus staticCorpus) SearchableDocuments(context.Context) ([]knowledge.Document, error) {
	return corpus.documents, nil
}

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
}

func TestHybridSearchCombinesSemanticAndLexicalScores(t *testing.T) {
	retriever := NewHybrid(staticCorpus{documents: []knowledge.Document{
		{SourceID: "semantic", Title: "Persistent daemon", Content: "Prompt hooks call one resident service.", Vector: []float32{1, 0}},
		{SourceID: "unrelated", Title: "Database", Content: "SQLite storage", Vector: []float32{0, 1}},
	}})
	hits, err := retriever.Search(context.Background(), "resident daemon hook", []float32{1, 0}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc.SourceID != "semantic" {
		t.Fatalf("unexpected hits: %#v", hits)
	}
}
