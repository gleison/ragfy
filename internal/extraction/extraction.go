package extraction

import (
	"context"

	"ragfy/internal/knowledge"
)

type Request struct {
	SessionID string
	Project   string
	Spine     string
}

type Extractor interface {
	Extract(context.Context, Request) ([]knowledge.Document, error)
}
