package reconciliation

import (
	"context"
	"testing"

	"ragfy/internal/knowledge"
)

type fakeGenerator struct {
	responses []string
	calls     int
}

func (generator *fakeGenerator) Generate(context.Context, string) (string, error) {
	response := generator.responses[generator.calls]
	generator.calls++
	return response, nil
}

func TestGenerativeReconcilerAcceptsOnlyCompatibleKnownRecords(t *testing.T) {
	newDocument := knowledge.Document{SourceID: "new", Kind: "fact", Scope: knowledge.ScopeProject, Project: "ragfy", Title: "New", Content: "new state"}
	existing := knowledge.Document{SourceID: "old", Kind: "fact", Scope: knowledge.ScopeProject, Project: "ragfy", Title: "Old", Content: "old state"}
	generator := &fakeGenerator{responses: []string{`{"actions":[{"new_source_id":"new","existing_source_id":"old","relation":"supersedes"}]}`}}
	actions, err := NewGenerative(generator).Reconcile(context.Background(), Request{Project: "ragfy", New: []knowledge.Document{newDocument}, Existing: []knowledge.Document{existing}})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Relation != RelationSupersedes {
		t.Fatalf("actions=%#v", actions)
	}
}

func TestGenerativeReconcilerRetriesInvalidJSONAndRejectsProblems(t *testing.T) {
	problem := knowledge.Document{SourceID: "new", Kind: "problem", Scope: knowledge.ScopeProject, Project: "ragfy"}
	existing := knowledge.Document{SourceID: "old", Kind: "problem", Scope: knowledge.ScopeProject, Project: "ragfy"}
	generator := &fakeGenerator{responses: []string{`not json`, `{"actions":[{"new_source_id":"new","existing_source_id":"old","relation":"duplicate"}]}`}}
	if _, err := NewGenerative(generator).Reconcile(context.Background(), Request{New: []knowledge.Document{problem}, Existing: []knowledge.Document{existing}}); err == nil {
		t.Fatal("reconciler accepted a problem supersession")
	}
	if generator.calls != 2 {
		t.Fatalf("generator calls=%d want=2", generator.calls)
	}
}
