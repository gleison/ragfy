package retrieval

import (
	"context"

	"ragfy/internal/knowledge"
)

type Corpus interface {
	SearchableDocuments(context.Context) ([]knowledge.Document, error)
}
