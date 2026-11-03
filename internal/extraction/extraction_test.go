package extraction

import (
	"context"
	"fmt"
	"testing"
)

type responseGenerator struct {
	responses []string
	calls     int
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
	evidence := "Ragfy runs as a persistent local Go daemon."
	validResponse := fmt.Sprintf(`{"problems":[],"facts":[{"fact":%q,"scope":"runtime","status":"current","evidence":%q}],"preferences":[],"decisions":[]}`, evidence, evidence)
	generator := &responseGenerator{responses: []string{"{", validResponse}}
	extractor := NewGenerative(generator)
	documents, err := extractor.Extract(context.Background(), Request{SessionID: "session", Project: "ragfy", Spine: "USER: " + evidence})
	if err != nil {
		t.Fatal(err)
	}
	if generator.calls != 2 || len(documents) != 1 || documents[0].Kind != "fact" {
		t.Fatalf("calls=%d documents=%#v", generator.calls, documents)
	}
}
