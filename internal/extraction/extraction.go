package extraction

import (
	"context"
	"fmt"
	"sort"

	"ragfy/internal/knowledge"
)

type Request struct {
	SessionID string
	Provider  string
	Project   string
	Spine     string
	Kind      string
	Title     string
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
	provider, err := router.ResolveProvider(request.Provider)
	if err != nil {
		return nil, err
	}
	request.Provider = provider
	extractor, ok := router.extractors[provider]
	if !ok || extractor == nil {
		return nil, fmt.Errorf("no knowledge extractor configured for provider %q", provider)
	}
	return extractor.Extract(ctx, request)
}

func (router *Router) ResolveProvider(requested string) (string, error) {
	if requested != "" {
		if router.extractors[requested] == nil {
			return "", fmt.Errorf("no knowledge extractor configured for provider %q", requested)
		}
		return requested, nil
	}
	for _, preferred := range []string{"codex", "claude"} {
		if router.extractors[preferred] != nil {
			return preferred, nil
		}
	}
	configured := make([]string, 0, len(router.extractors))
	for source, extractor := range router.extractors {
		if extractor != nil {
			configured = append(configured, source)
		}
	}
	sort.Strings(configured)
	if len(configured) == 0 {
		return "", fmt.Errorf("no knowledge extractor is configured")
	}
	return configured[0], nil
}
