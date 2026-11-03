package extraction

import (
	"context"
	"fmt"
	"log"

	"ragfy/internal/digest"
	"ragfy/internal/knowledge"
	"ragfy/internal/model"
)

type Generative struct {
	generator model.Generator
}

var _ Extractor = (*Generative)(nil)

func NewGenerative(generator model.Generator) *Generative {
	return &Generative{generator: generator}
}

func (extractor *Generative) Extract(ctx context.Context, request Request) ([]knowledge.Document, error) {
	response, err := extractor.generator.Generate(ctx, digest.Prompt(request.Project, request.Spine))
	if err != nil {
		return nil, err
	}
	documents, err := digest.ParseRecords(response, request.Spine)
	if err == nil {
		return documents, nil
	}
	log.Printf("digest %s: invalid structured response (%v); retrying once with strict JSON instructions", request.SessionID, err)
	retryPrompt := "Return one valid JSON object only. It must start with { and end with }. Do not add analysis, explanations, or markdown.\n\n" + digest.Prompt(request.Project, request.Spine)
	response, err = extractor.generator.Generate(ctx, retryPrompt)
	if err != nil {
		return nil, err
	}
	documents, err = digest.ParseRecords(response, request.Spine)
	if err != nil {
		return nil, fmt.Errorf("digest model response did not contain valid JSON after retry: %w", err)
	}
	return documents, nil
}
