package retrieval

import (
	"context"

	"ragfy/internal/knowledge"
)

type Retriever interface {
	Search(context.Context, string, []float32, int) ([]knowledge.Hit, error)
}
