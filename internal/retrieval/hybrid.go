package retrieval

import (
	"context"
	"log"
	"math"
	"strings"
	"unicode"

	"ragfy/internal/knowledge"
)

const (
	maximumCandidates = 12
	semanticWeight    = 0.68
	lexicalWeight     = 0.28
	projectBoost      = 0.04
	relativeCutoff    = 0.75
	lexicalCutoff     = 0.60
	minimumScore      = 0.38
	diversityWeight   = 0.82
)

var stopWords = map[string]bool{
	"about": true, "and": true, "are": true, "but": true, "can": true, "for": true, "from": true,
	"has": true, "have": true, "how": true, "into": true, "make": true, "need": true, "not": true,
	"please": true, "that": true, "the": true, "this": true, "use": true, "using": true, "want": true,
	"was": true, "what": true, "why": true, "with": true, "you": true, "your": true,
	"aos": true, "com": true, "como": true, "das": true, "de": true, "dos": true, "ela": true, "ele": true,
	"eles": true, "essa": true, "esse": true, "esta": true, "este": true, "isso": true, "mais": true,
	"não": true, "nos": true, "para": true, "pela": true, "pelo": true, "por": true, "que": true,
	"ser": true, "sem": true, "uma": true, "você": true,
}

type Hybrid struct {
	corpus Corpus
}

type documentTerms struct {
	title   []string
	content []string
}

var _ Retriever = (*Hybrid)(nil)

func NewHybrid(corpus Corpus) *Hybrid {
	return &Hybrid{corpus: corpus}
}

func (retriever *Hybrid) Search(ctx context.Context, query Query) ([]knowledge.Hit, error) {
	if query.Limit <= 0 {
		return nil, nil
	}
	documents, err := retriever.corpus.SearchableDocuments(ctx)
	if err != nil {
		return nil, err
	}
	documents = visibleDocuments(documents, query.Project)
	lexicalScores := bm25Scores(queryTermsForProject(query.Text, query.Project), documents)
	candidates := make([]knowledge.Hit, 0, len(documents))
	for index, document := range documents {
		semanticScore := cosine(query.Vector, document.Vector)
		lexicalScore := lexicalScores[index]
		if !candidateIsRelevant(semanticScore, lexicalScore) {
			continue
		}
		boost := 0.0
		if document.Scope == knowledge.ScopeProject && document.Project == query.Project {
			boost = projectBoost
		}
		candidates = append(candidates, knowledge.Hit{
			Score:         semanticScore*semanticWeight + lexicalScore*lexicalWeight + boost,
			SemanticScore: semanticScore,
			LexicalScore:  lexicalScore,
			ProjectBoost:  boost,
			Doc:           document,
		})
	}
	sortHits(candidates)
	if len(candidates) > maximumCandidates {
		candidates = candidates[:maximumCandidates]
	}
	eligible := relativelyRelevant(candidates)
	selected := diverseHits(eligible, query.Limit)
	logRanking(query.Project, candidates, eligible, selected)
	return selected, nil
}

func visibleDocuments(documents []knowledge.Document, project string) []knowledge.Document {
	visible := make([]knowledge.Document, 0, len(documents))
	for _, document := range documents {
		if document.Scope == knowledge.ScopeGlobal || document.Scope == knowledge.ScopeProject && document.Project == project {
			visible = append(visible, document)
		}
	}
	return visible
}

func candidateIsRelevant(semanticScore, lexicalScore float64) bool {
	return semanticScore >= 0.32 && (semanticScore >= 0.45 || lexicalScore >= 0.12) || lexicalScore >= 0.55
}

func relativelyRelevant(candidates []knowledge.Hit) []knowledge.Hit {
	if len(candidates) == 0 {
		return nil
	}
	cutoff := math.Max(minimumScore, candidates[0].Score*relativeCutoff)
	lexicalThreshold := math.Max(minimumScore, candidates[0].Score*lexicalCutoff)
	eligible := make([]knowledge.Hit, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Score >= cutoff || candidate.LexicalScore > 0 && candidate.Score >= lexicalThreshold {
			eligible = append(eligible, candidate)
		}
	}
	return eligible
}

func diverseHits(candidates []knowledge.Hit, limit int) []knowledge.Hit {
	remaining := append([]knowledge.Hit(nil), candidates...)
	selected := make([]knowledge.Hit, 0, min(limit, len(candidates)))
	for len(remaining) > 0 && len(selected) < limit {
		bestIndex := 0
		bestScore := math.Inf(-1)
		for index, candidate := range remaining {
			redundancy := maximumSimilarity(candidate, selected)
			score := diversityWeight*candidate.Score - (1-diversityWeight)*redundancy
			if score > bestScore {
				bestIndex, bestScore = index, score
			}
		}
		selected = append(selected, remaining[bestIndex])
		remaining = append(remaining[:bestIndex], remaining[bestIndex+1:]...)
	}
	return selected
}

func maximumSimilarity(candidate knowledge.Hit, selected []knowledge.Hit) float64 {
	maximum := 0.0
	for _, existing := range selected {
		maximum = math.Max(maximum, cosine(candidate.Doc.Vector, existing.Doc.Vector))
	}
	return maximum
}

func bm25Scores(query []string, documents []knowledge.Document) []float64 {
	scores := make([]float64, len(documents))
	if len(query) == 0 || len(documents) == 0 {
		return scores
	}
	terms, averageTitleLength, averageContentLength := indexTerms(documents)
	for _, queryTerm := range query {
		documentFrequency := 0
		for _, document := range terms {
			if termFrequency(queryTerm, document.title)+termFrequency(queryTerm, document.content) > 0 {
				documentFrequency++
			}
		}
		inverseFrequency := math.Log(1 + (float64(len(documents)-documentFrequency)+0.5)/(float64(documentFrequency)+0.5))
		for index, document := range terms {
			titleScore := saturatedFrequency(termFrequency(queryTerm, document.title), len(document.title), averageTitleLength, 0.3)
			contentScore := saturatedFrequency(termFrequency(queryTerm, document.content), len(document.content), averageContentLength, 0.75)
			scores[index] += inverseFrequency * (titleScore*2.4 + contentScore)
		}
	}
	normalizeScores(scores)
	return scores
}

func indexTerms(documents []knowledge.Document) ([]documentTerms, float64, float64) {
	indexed := make([]documentTerms, len(documents))
	titleTerms, contentTerms := 0, 0
	for index, document := range documents {
		indexed[index] = documentTerms{title: tokenize(document.Title, false), content: tokenize(document.Content, false)}
		titleTerms += len(indexed[index].title)
		contentTerms += len(indexed[index].content)
	}
	count := float64(len(documents))
	return indexed, math.Max(1, float64(titleTerms)/count), math.Max(1, float64(contentTerms)/count)
}

func saturatedFrequency(frequency, length int, averageLength, normalization float64) float64 {
	if frequency == 0 {
		return 0
	}
	const saturation = 1.2
	normalizedLength := 1 - normalization + normalization*float64(length)/averageLength
	return float64(frequency) * (saturation + 1) / (float64(frequency) + saturation*normalizedLength)
}

func termFrequency(term string, terms []string) int {
	frequency := 0
	for _, candidate := range terms {
		if termsMatch(term, candidate) {
			frequency++
		}
	}
	return frequency
}

func termsMatch(first, second string) bool {
	if first == second {
		return true
	}
	if len(first) < 6 || len(second) < 6 {
		return false
	}
	common := 0
	for common < len(first) && common < len(second) && first[common] == second[common] {
		common++
	}
	return common >= 6
}

func normalizeScores(scores []float64) {
	maximum := 0.0
	for _, score := range scores {
		maximum = math.Max(maximum, score)
	}
	if maximum == 0 {
		return
	}
	for index := range scores {
		scores[index] /= maximum
	}
}

func queryTerms(query string) []string {
	return tokenize(query, true)
}

func queryTermsForProject(query, project string) []string {
	projectTerms := make(map[string]bool)
	for _, term := range tokenize(project, true) {
		projectTerms[term] = true
	}
	var terms []string
	for _, term := range queryTerms(query) {
		if !projectTerms[term] {
			terms = append(terms, term)
		}
	}
	return terms
}

func tokenize(text string, unique bool) []string {
	var terms []string
	seen := make(map[string]bool)
	for _, term := range strings.FieldsFunc(strings.ToLower(text), func(character rune) bool {
		return !(unicode.IsLetter(character) || unicode.IsNumber(character) || character == '_' || character == '-')
	}) {
		if len(term) < 3 || stopWords[term] || unique && seen[term] {
			continue
		}
		terms = append(terms, term)
		seen[term] = true
	}
	return terms
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
	return dotProduct / (math.Sqrt(firstMagnitude) * math.Sqrt(secondMagnitude))
}

func sortHits(hits []knowledge.Hit) {
	for index := 1; index < len(hits); index++ {
		for position := index; position > 0 && hits[position].Score > hits[position-1].Score; position-- {
			hits[position], hits[position-1] = hits[position-1], hits[position]
		}
	}
}

func logRanking(project string, candidates, eligible, selected []knowledge.Hit) {
	log.Printf("recall ranking project=%s candidates=%d eligible=%d selected=%d", project, len(candidates), len(eligible), len(selected))
	eligibleIDs := hitIDs(eligible)
	selectedIDs := hitIDs(selected)
	for rank, hit := range candidates {
		status := "cutoff"
		if eligibleIDs[hit.Doc.Source+"\n"+hit.Doc.SourceID] {
			status = "eligible"
		}
		if selectedIDs[hit.Doc.Source+"\n"+hit.Doc.SourceID] {
			status = "selected"
		}
		log.Printf("recall candidate_rank=%d source_id=%s kind=%s scope=%s status=%s score=%.4f semantic=%.4f lexical=%.4f project_boost=%.2f", rank+1, hit.Doc.SourceID, hit.Doc.Kind, hit.Doc.Scope, status, hit.Score, hit.SemanticScore, hit.LexicalScore, hit.ProjectBoost)
	}
	for rank, hit := range selected {
		log.Printf("recall selected_rank=%d source_id=%s", rank+1, hit.Doc.SourceID)
	}
}

func hitIDs(hits []knowledge.Hit) map[string]bool {
	identifiers := make(map[string]bool, len(hits))
	for _, hit := range hits {
		identifiers[hit.Doc.Source+"\n"+hit.Doc.SourceID] = true
	}
	return identifiers
}
