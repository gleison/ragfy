package extraction

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	"ragfy/internal/digest"
	"ragfy/internal/knowledge"
)

type responseGenerator struct {
	responses []string
	prompts   []string
	calls     int
}

type recordingExtractor struct {
	request Request
}

func (extractor *recordingExtractor) Extract(_ context.Context, request Request) ([]knowledge.Document, error) {
	extractor.request = request
	return []knowledge.Document{{Kind: request.Source}}, nil
}

func (generator *responseGenerator) Generate(_ context.Context, prompt string) (string, error) {
	if generator.calls >= len(generator.responses) {
		return "", fmt.Errorf("unexpected generation call %d", generator.calls+1)
	}
	generator.prompts = append(generator.prompts, prompt)
	response := generator.responses[generator.calls]
	generator.calls++
	return response, nil
}

func TestGenerativeConsolidatesOverlappingWindows(t *testing.T) {
	evidence := "O código deve usar métodos atômicos e nomes significativos."
	spine := strings.Join([]string{
		"USER: Initial context for the architecture discussion.",
		"CODEX: Prefer separate interfaces for replaceable implementations.",
		"USER: De acordo. Pode seguir com essa arquitetura.",
		"CODEX: More implementation context that makes this transcript longer.",
		"USER: " + evidence,
	}, "\n\n")

	extractor := NewGenerative(nil)
	extractor.windowCharacters = 125
	extractor.overlapCharacters = 45
	windows := digest.TranscriptWindows(spine, extractor.windowCharacters, extractor.overlapCharacters)
	if len(windows) < 2 {
		t.Fatalf("test spine did not produce multiple windows: %#v", windows)
	}
	empty := `{"problems":[],"facts":[],"preferences":[],"decisions":[]}`
	final := fmt.Sprintf(`{"problems":[],"facts":[],"preferences":[{"preference":"Preferir métodos atômicos e nomes significativos.","why":"","applies_to":"código","evidence":%q,"acceptance":"explicit","acceptance_evidence":"","visibility":"global"}],"decisions":[]}`, evidence)
	responses := make([]string, len(windows)+1)
	for index := range windows {
		responses[index] = empty
	}
	responses[len(responses)-1] = final
	generator := &responseGenerator{responses: responses}
	extractor.generator = generator

	documents, err := extractor.Extract(context.Background(), Request{SessionID: "windowed", Source: "codex", Project: "ragfy", Spine: spine})
	if err != nil {
		t.Fatal(err)
	}
	if generator.calls != len(windows)+1 || len(documents) != 1 || documents[0].Kind != "preference" {
		t.Fatalf("calls=%d windows=%d documents=%#v", generator.calls, len(windows), documents)
	}
	if !strings.Contains(generator.prompts[len(generator.prompts)-1], "Consolidate candidate knowledge") {
		t.Fatalf("last prompt was not consolidation: %s", generator.prompts[len(generator.prompts)-1])
	}
}

func TestGenerativeRetriesInvalidResponse(t *testing.T) {
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousOutput)

	evidence := "Ragfy runs as a persistent local Go daemon."
	validResponse := fmt.Sprintf(`{"problems":[],"facts":[{"fact":%q,"area":"runtime","status":"current","evidence":%q,"visibility":"project"}],"preferences":[],"decisions":[]}`, evidence, evidence)
	generator := &responseGenerator{responses: []string{"{", validResponse}}
	extractor := NewGenerative(generator)
	documents, err := extractor.Extract(context.Background(), Request{SessionID: "session", Source: "codex", Project: "ragfy", Spine: "USER: " + evidence})
	if err != nil {
		t.Fatal(err)
	}
	if generator.calls != 2 || len(documents) != 1 || documents[0].Kind != "fact" {
		t.Fatalf("calls=%d documents=%#v", generator.calls, documents)
	}
	for _, want := range []string{
		"invalid structured response",
		"extraction source=codex windows=1 candidates=1 accepted=1 rejected=0",
		"candidates_by_kind=fact:1 accepted_by_kind=fact:1 accepted_by_scope=project:1 rejection_reasons=none",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log missing %q:\n%s", want, logs.String())
		}
	}
}

func TestRouterSelectsExtractorByExplicitSource(t *testing.T) {
	codex := &recordingExtractor{}
	claude := &recordingExtractor{}
	router := NewRouter(map[string]Extractor{"codex": codex, "claude": claude})
	documents, err := router.Extract(context.Background(), Request{SessionID: "s1", Source: "claude", Spine: "content"})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Kind != "claude" || claude.request.SessionID != "s1" {
		t.Fatalf("unexpected routed extraction: documents=%#v request=%#v", documents, claude.request)
	}
	if codex.request.SessionID != "" {
		t.Fatalf("codex extractor was unexpectedly called: %#v", codex.request)
	}
	if _, err := router.Extract(context.Background(), Request{Source: "unknown"}); err == nil {
		t.Fatal("router accepted an unconfigured source")
	}
}
