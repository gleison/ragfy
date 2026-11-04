package extraction

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	"ragfy/internal/knowledge"
)

type responseGenerator struct {
	responses []string
	calls     int
}

type recordingExtractor struct {
	request Request
}

func (extractor *recordingExtractor) Extract(_ context.Context, request Request) ([]knowledge.Document, error) {
	extractor.request = request
	return []knowledge.Document{{Kind: request.Source}}, nil
}

func (generator *responseGenerator) Generate(context.Context, string) (string, error) {
	if generator.calls >= len(generator.responses) {
		return "", fmt.Errorf("unexpected generation call %d", generator.calls+1)
	}
	response := generator.responses[generator.calls]
	generator.calls++
	return response, nil
}

func TestGenerativeRetriesInvalidResponse(t *testing.T) {
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousOutput)

	evidence := "Ragfy runs as a persistent local Go daemon."
	validResponse := fmt.Sprintf(`{"problems":[],"facts":[{"fact":%q,"scope":"runtime","status":"current","evidence":%q}],"preferences":[],"decisions":[]}`, evidence, evidence)
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
		"extraction source=codex candidates=1 accepted=1 rejected=0",
		"candidates_by_kind=fact:1 accepted_by_kind=fact:1 rejection_reasons=none",
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
