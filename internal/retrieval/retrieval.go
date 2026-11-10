package retrieval

import (
	"context"

	"ragfy/internal/knowledge"
)

type Retriever interface {
	Search(context.Context, Query) ([]knowledge.Hit, error)
	Explain(context.Context, Query) (Explanation, error)
}

const (
	CandidateSelected = "selected"
	CandidateEligible = "eligible"
	CandidateCutoff   = "cutoff"
)

type Candidate struct {
	Rank         int
	SelectedRank int
	Status       string
	Hit          knowledge.Hit
}

type Explanation struct {
	Selected      []knowledge.Hit
	Candidates    []Candidate
	ScoreCutoff   float64
	LexicalCutoff float64
}

type Query struct {
	Text    string
	Vector  []float32
	Limit   int
	Project string
}
