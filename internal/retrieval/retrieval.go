package retrieval

import (
	"context"

	"ragfy/internal/knowledge"
)

type Retriever interface {
	Search(context.Context, Query) ([]knowledge.Hit, error)
}

type Query struct {
	Text    string
	Vector  []float32
	Limit   int
	Project string
}
