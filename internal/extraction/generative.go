package extraction

import (
	"context"
	"fmt"
	"log"
	"strings"
	"unicode"
	"unicode/utf8"

	"ragfy/internal/digest"
	"ragfy/internal/knowledge"
	"ragfy/internal/model"
)

type Generative struct {
	generator         model.Generator
	windowCharacters  int
	overlapCharacters int
	local             bool
}

const (
	defaultWindowCharacters  = 120_000
	defaultOverlapCharacters = 12_000
	localWindowBytes         = 20_000
	localOverlapBytes        = 2_000
)

var _ Extractor = (*Generative)(nil)

func NewGenerative(generator model.Generator) *Generative {
	return &Generative{
		generator:         generator,
		windowCharacters:  defaultWindowCharacters,
		overlapCharacters: defaultOverlapCharacters,
	}
}

func NewLocalGenerative(generator model.Generator) *Generative {
	return &Generative{generator: generator, windowCharacters: localWindowBytes, overlapCharacters: localOverlapBytes, local: true}
}

func (extractor *Generative) Extract(ctx context.Context, request Request) ([]knowledge.Document, error) {
	windows := extractor.extractionWindows(request)
	responses := make([]string, 0, len(windows))
	for index, window := range windows {
		label := fmt.Sprintf("window %d/%d", index+1, len(windows))
		prompt := extractor.extractionPrompt(request, window)
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
		response, err = extractor.consolidate(ctx, request, responses)
		if err != nil {
			return nil, err
		}
	}

	documents, audit, err := digest.ParseRecordsWithAudit(response, request.Spine)
	if err != nil {
		return nil, fmt.Errorf("validate final digest response: %w", err)
	}
	if extractor.local {
		assignProjectScope(documents, request.Project, audit.AcceptedScopes)
	}
	log.Printf("digest %s: extraction provider=%s windows=%d %s", request.SessionID, request.Provider, len(windows), audit.Summary())
	return documents, nil
}

func (extractor *Generative) extractionWindows(request Request) []string {
	windows := digest.TranscriptWindows(request.Spine, extractor.windowCharacters, extractor.overlapCharacters)
	if !extractor.local {
		return windows
	}
	var boundedWindows []string
	for _, window := range windows {
		boundedWindows = append(boundedWindows, splitLocalWindow(window, isSession(request))...)
	}
	return boundedWindows
}

func (extractor *Generative) extractionPrompt(request Request, window string) string {
	var prompt string
	if isSession(request) {
		prompt = digest.Prompt(request.Project, window)
	} else {
		prompt = digest.SourcePrompt(request.Project, request.Kind, request.Title, window)
	}
	if extractor.local {
		return localExtractionGuidance + "\n\n" + prompt
	}
	return prompt
}

func isSession(request Request) bool {
	return request.Kind == "" || request.Kind == "session"
}

func assignProjectScope(documents []knowledge.Document, project string, scopeCounts map[string]int) {
	if project == "" {
		return
	}
	for index := range documents {
		document := &documents[index]
		if document.Kind == "fact" && document.Scope == knowledge.ScopeGlobal && factNamesProject(*document, project) {
			document.Scope = knowledge.ScopeProject
			scopeCounts[string(knowledge.ScopeGlobal)]--
			scopeCounts[string(knowledge.ScopeProject)]++
		}
	}
}

const localExtractionGuidance = `Scope examples:
- "This repository uses SQLite and stores data in memory.sqlite." is project knowledge.
- "The project reuses its embedding runtime for local extraction." is project knowledge even if the same technology can be used elsewhere.
- "I prefer concise answers in all repositories." is a global preference.
For each record, use exactly "project" or "global" for visibility. Use "project" for architecture, models, configuration, and behavior specific to this project, including claims about its local tools. Use "global" only if the claim itself applies outside the project. If unsure, choose "project".
Capture explicit user choices about architecture, dependencies, and defaults as decisions. Do not repeat a choice as a fact. Combine paraphrases of one claim into one record with its strongest exact evidence.`

func factNamesProject(document knowledge.Document, project string) bool {
	claim, _, _ := strings.Cut(document.Content, "\n")
	project = strings.ToLower(project)
	words := func(value string) []string {
		return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
		})
	}
	for _, word := range words(claim) {
		if word == project {
			return true
		}
	}
	evidence, _ := document.Metadata["evidence"].(string)
	subject := strings.TrimLeft(strings.ToLower(strings.TrimSpace(evidence)), "`")
	for _, article := range []string{"the ", "o ", "a "} {
		subject = strings.TrimPrefix(subject, article)
	}
	tokens := words(subject)
	return len(tokens) > 0 && tokens[0] == project
}

func (extractor *Generative) consolidate(ctx context.Context, request Request, responses []string) (string, error) {
	prompt := digest.ConsolidationPrompt(request.Project, responses)
	if !extractor.local || len(prompt) <= localWindowBytes {
		return extractor.generateJSON(ctx, request.SessionID, "consolidation", prompt, request.Spine)
	}
	combined := responses[0]
	for _, next := range responses[1:] {
		prompt := digest.ConsolidationPrompt(request.Project, []string{combined, next})
		var err error
		combined, err = extractor.generateJSON(ctx, request.SessionID, "consolidation", prompt, request.Spine)
		if err != nil {
			return "", err
		}
	}
	return combined, nil
}

func splitLocalWindow(window string, transcript bool) []string {
	var speaker string
	if transcript {
		for _, prefix := range []string{"USER: ", "CODEX: ", "CLAUDE: "} {
			if strings.HasPrefix(window, prefix) {
				speaker = prefix
				break
			}
		}
	}
	var parts []string
	for len(window) > localWindowBytes {
		end := localWindowBytes
		for end > localWindowBytes-utf8.UTFMax+1 && !utf8.RuneStart(window[end]) {
			end--
		}
		parts = append(parts, window[:end])
		overlap := end - localOverlapBytes
		start := overlap
		for start > overlap-utf8.UTFMax+1 && !utf8.RuneStart(window[start]) {
			start--
		}
		window = speaker + window[start:]
	}
	return append(parts, window)
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
