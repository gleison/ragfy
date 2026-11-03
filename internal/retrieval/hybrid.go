package retrieval

import (
	"context"
	"strings"
	"unicode"

	"ragfy/internal/knowledge"
)

type Hybrid struct {
	corpus Corpus
}

var _ Retriever = (*Hybrid)(nil)

func NewHybrid(corpus Corpus) *Hybrid {
	return &Hybrid{corpus: corpus}
}

func (retriever *Hybrid) Search(ctx context.Context, query string, queryVector []float32, limit int) ([]knowledge.Hit, error) {
	documents, err := retriever.corpus.SearchableDocuments(ctx)
	if err != nil {
		return nil, err
	}
	terms := queryTerms(query)
	hits := make([]knowledge.Hit, 0)
	for _, document := range documents {
		semanticScore := cosine(queryVector, document.Vector)
		lexicalMatch := lexicalScore(terms, document.Title+" "+document.Content)
		if semanticScore < .35 || (semanticScore < .50 && lexicalMatch == 0) {
			continue
		}
		hits = append(hits, knowledge.Hit{Score: semanticScore*.75 + lexicalMatch*.25, Doc: document})
	}
	sortHits(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func queryTerms(query string) []string {
	stopWords := map[string]bool{"the": true, "and": true, "for": true, "you": true, "your": true, "that": true, "this": true, "with": true, "from": true, "are": true, "was": true, "what": true, "how": true, "why": true, "can": true, "please": true, "about": true, "have": true, "has": true, "into": true, "not": true, "but": true, "use": true, "using": true, "want": true, "need": true, "make": true}
	var terms []string
	seen := map[string]bool{}
	for _, term := range strings.FieldsFunc(strings.ToLower(query), func(character rune) bool {
		return !(unicode.IsLetter(character) || unicode.IsNumber(character) || character == '_' || character == '-' || character == '.')
	}) {
		if len(term) < 3 || stopWords[term] || seen[term] {
			continue
		}
		terms = append(terms, term)
		seen[term] = true
	}
	return terms
}

func lexicalScore(terms []string, text string) float64 {
	if len(terms) == 0 {
		return 0
	}
	lowerText := strings.ToLower(text)
	matches := 0
	for _, term := range terms {
		if strings.Contains(lowerText, term) {
			matches++
		}
	}
	return float64(matches) / float64(len(terms))
}

func cosine(first, second []float32) float64 {
	if len(first) == 0 || len(first) != len(second) {
		return 0
	}
	var dotProduct, firstMagnitude, secondMagnitude float64
	for index := range first {
		firstValue, secondValue := float64(first[index]), float64(second[index])
		dotProduct += firstValue * secondValue
		firstMagnitude += firstValue * firstValue
		secondMagnitude += secondValue * secondValue
	}
	if firstMagnitude == 0 || secondMagnitude == 0 {
		return 0
	}
	return dotProduct / (firstMagnitude * secondMagnitude)
}

func sortHits(hits []knowledge.Hit) {
	for index := 1; index < len(hits); index++ {
		for position := index; position > 0 && hits[position].Score > hits[position-1].Score; position-- {
			hits[position], hits[position-1] = hits[position-1], hits[position]
		}
	}
}
