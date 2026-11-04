package extraction

import (
	"context"
	"fmt"

	"ragfy/internal/knowledge"
)

type Request struct {
	SessionID string
	Source    string
	Project   string
	Spine     string
}

type Extractor interface {
	Extract(context.Context, Request) ([]knowledge.Document, error)
}

type Router struct {
	extractors map[string]Extractor
}

var _ Extractor = (*Router)(nil)

func NewRouter(extractors map[string]Extractor) *Router {
	configured := make(map[string]Extractor, len(extractors))
	for source, extractor := range extractors {
		configured[source] = extractor
	}
	return &Router{extractors: configured}
}

func (router *Router) Extract(ctx context.Context, request Request) ([]knowledge.Document, error) {
	extractor, ok := router.extractors[request.Source]
	if !ok || extractor == nil {
		return nil, fmt.Errorf("no knowledge extractor configured for source %q", request.Source)
	}
	return extractor.Extract(ctx, request)
}
