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
	generator         model.Generator
	windowCharacters  int
	overlapCharacters int
}

const (
	defaultWindowCharacters  = 120_000
	defaultOverlapCharacters = 12_000
)

var _ Extractor = (*Generative)(nil)

func NewGenerative(generator model.Generator) *Generative {
	return &Generative{
		generator:         generator,
		windowCharacters:  defaultWindowCharacters,
		overlapCharacters: defaultOverlapCharacters,
	}
}

func (extractor *Generative) Extract(ctx context.Context, request Request) ([]knowledge.Document, error) {
	windows := digest.TranscriptWindows(request.Spine, extractor.windowCharacters, extractor.overlapCharacters)
	responses := make([]string, 0, len(windows))
	for index, window := range windows {
		label := fmt.Sprintf("window %d/%d", index+1, len(windows))
		prompt := digest.Prompt(request.Project, window)
		if request.Kind != "" && request.Kind != "session" {
			prompt = digest.SourcePrompt(request.Project, request.Kind, request.Title, window)
		}
		response, err := extractor.generateJSON(ctx, request.SessionID, label, prompt, window)
		if err != nil {
			return nil, err
		}
		responses = append(responses, response)
	}

	response := responses[0]
	if len(responses) > 1 {
		log.Printf("digest %s: consolidating extraction provider=%s windows=%d spine_characters=%d", request.SessionID, request.Provider, len(responses), len(request.Spine))
		var err error
		response, err = extractor.generateJSON(ctx, request.SessionID, "consolidation", digest.ConsolidationPrompt(request.Project, responses), request.Spine)
		if err != nil {
			return nil, err
		}
	}

	documents, audit, err := digest.ParseRecordsWithAudit(response, request.Spine)
	if err != nil {
		return nil, fmt.Errorf("validate final digest response: %w", err)
	}
	log.Printf("digest %s: extraction provider=%s windows=%d %s", request.SessionID, request.Provider, len(windows), audit.Summary())
	return documents, nil
}

func (extractor *Generative) generateJSON(ctx context.Context, sessionID, label, prompt, evidenceSpine string) (string, error) {
	response, err := extractor.generator.Generate(ctx, prompt)
	if err != nil {
		return "", err
	}
	if _, _, err = digest.ParseRecordsWithAudit(response, evidenceSpine); err == nil {
		return response, nil
	}

	log.Printf("digest %s: invalid structured response in %s (%v); retrying once with strict JSON instructions", sessionID, label, err)
	retryPrompt := "Return one valid JSON object only. It must start with { and end with }. Do not add analysis, explanations, or markdown.\n\n" + prompt
	response, err = extractor.generator.Generate(ctx, retryPrompt)
	if err != nil {
		return "", err
	}
	if _, _, err = digest.ParseRecordsWithAudit(response, evidenceSpine); err != nil {
		return "", fmt.Errorf("digest model response for %s did not contain valid JSON after retry: %w", label, err)
	}
	return response, nil
}
