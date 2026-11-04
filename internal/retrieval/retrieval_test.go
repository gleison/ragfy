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
	for _, expected := range []string{"serviço", "persistente", "evita", "recarregamento", "multilíngue"} {
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

func TestQueryTermsExcludeCurrentProjectAndMatchMorphology(t *testing.T) {
	terms := queryTermsForProject("Melhore o retriever do Ragfy.", "ragfy")
	if strings.Contains(strings.Join(terms, "|"), "ragfy") {
		t.Fatalf("project leaked into query terms: %#v", terms)
	}
	if !termsMatch("retriever", "retrieval") || termsMatch("retrieve", "retrofit") {
		t.Fatal("unexpected morphological matching")
	}
}

func TestVectorAndRankingHelpers(t *testing.T) {
	if actual := cosine([]float32{1, 0}, []float32{1, 0}); math.Abs(actual-1) > 1e-9 {
		t.Fatalf("cosine identical = %f", actual)
	}
	if actual := cosine([]float32{1}, []float32{1, 0}); actual != 0 {
		t.Fatalf("cosine mismatched dimensions = %f", actual)
	}
	hits := []knowledge.Hit{{Score: 0.2}, {Score: 0.9}, {Score: 0.5}}
	sortHits(hits)
	if hits[0].Score != 0.9 || hits[2].Score != 0.2 {
		t.Fatalf("hits not sorted: %#v", hits)
	}
}

func TestBM25WeightsTitleAboveContent(t *testing.T) {
	documents := []knowledge.Document{
		{Title: "Retriever", Content: "Generic implementation details."},
		{Title: "Generic details", Content: "The retriever implementation."},
	}
	scores := bm25Scores([]string{"retriever"}, documents)
	if len(scores) != 2 || scores[0] <= scores[1] || scores[0] != 1 {
		t.Fatalf("scores=%#v", scores)
	}
}

func TestDiverseHitsAvoidsNearDuplicateResults(t *testing.T) {
	candidates := []knowledge.Hit{
		{Score: 0.90, Doc: knowledge.Document{SourceID: "best", Vector: []float32{1, 0}}},
		{Score: 0.88, Doc: knowledge.Document{SourceID: "duplicate", Vector: []float32{1, 0}}},
		{Score: 0.82, Doc: knowledge.Document{SourceID: "diverse", Vector: []float32{0, 1}}},
	}
	selected := diverseHits(candidates, 2)
	if len(selected) != 2 || selected[0].Doc.SourceID != "best" || selected[1].Doc.SourceID != "diverse" {
		t.Fatalf("selected=%#v", selected)
	}
}

func TestHybridSearchCombinesSemanticAndLexicalScores(t *testing.T) {
	retriever := NewHybrid(staticCorpus{documents: []knowledge.Document{
		{SourceID: "semantic", Title: "Persistent daemon", Content: "Prompt hooks call one resident service.", Scope: knowledge.ScopeGlobal, Vector: []float32{1, 0}},
		{SourceID: "unrelated", Title: "Database", Content: "SQLite storage", Scope: knowledge.ScopeGlobal, Vector: []float32{0, 1}},
	}})
	hits, err := retriever.Search(context.Background(), Query{Text: "resident daemon hook", Vector: []float32{1, 0}, Limit: 4, Project: "ragfy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Doc.SourceID != "semantic" {
		t.Fatalf("unexpected hits: %#v", hits)
	}
}

func TestHybridSearchCutsTangentialRetrieverKnowledge(t *testing.T) {
	retriever := NewHybrid(staticCorpus{documents: []knowledge.Document{
		{SourceID: "retriever", Title: "O retriever usa full scan", Content: "O retriever pode ser melhorado com outro índice.", Scope: knowledge.ScopeProject, Project: "ragfy", Vector: []float32{1, 0}},
		{SourceID: "hybrid", Title: "Ranking híbrido", Content: "O retriever combina similaridade, busca lexical e ranking.", Scope: knowledge.ScopeProject, Project: "ragfy", Vector: []float32{0.95, 0.31}},
		{SourceID: "qwen", Title: "Embedding Qwen", Content: "A instrução de embedding fica no adaptador.", Scope: knowledge.ScopeProject, Project: "ragfy", Vector: []float32{0.75, 0.66}},
		{SourceID: "hook", Title: "Injeção pelo hook do Ragfy", Content: "O hook do Ragfy anexa conhecimento ao prompt.", Scope: knowledge.ScopeProject, Project: "ragfy", Vector: []float32{0.7, 0.71}},
	}})
	hits, err := retriever.Search(context.Background(), Query{Text: "melhorar o retriever do Ragfy", Vector: []float32{1, 0}, Limit: 4, Project: "ragfy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Doc.SourceID != "retriever" || hits[1].Doc.SourceID != "hybrid" {
		t.Fatalf("hits=%#v", hits)
	}
	if hits[0].LexicalScore <= hits[1].LexicalScore || hits[0].ProjectBoost != projectBoost {
		t.Fatalf("unexpected score components: %#v", hits)
	}
}

func TestHybridSearchIncludesGlobalAndCurrentProjectOnly(t *testing.T) {
	retriever := NewHybrid(staticCorpus{documents: []knowledge.Document{
		{SourceID: "global", Title: "Atomic methods", Content: "Use small atomic methods.", Scope: knowledge.ScopeGlobal, Vector: []float32{1, 0}},
		{SourceID: "ragfy", Title: "Ragfy architecture", Content: "Use a persistent service.", Scope: knowledge.ScopeProject, Project: "ragfy", Vector: []float32{1, 0}},
		{SourceID: "other", Title: "Other architecture", Content: "Use a remote service.", Scope: knowledge.ScopeProject, Project: "other", Vector: []float32{1, 0}},
	}})
	hits, err := retriever.Search(context.Background(), Query{Text: "service methods architecture", Vector: []float32{1, 0}, Limit: 4, Project: "ragfy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits=%#v", hits)
	}
	for _, hit := range hits {
		if hit.Doc.SourceID == "other" {
			t.Fatalf("other project leaked into results: %#v", hits)
		}
	}
}
