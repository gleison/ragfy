package reconciliation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ragfy/internal/knowledge"
	"ragfy/internal/model"
)

const JSONSchema = `{"type":"object","additionalProperties":false,"required":["actions"],"properties":{"actions":{"type":"array","maxItems":64,"items":{"type":"object","additionalProperties":false,"required":["new_source_id","existing_source_id","relation"],"properties":{"new_source_id":{"type":"string"},"existing_source_id":{"type":"string"},"relation":{"type":"string","enum":["duplicate","supersedes"]}}}}}}`

type Generative struct {
	generator model.Generator
}

var _ Reconciler = (*Generative)(nil)

func NewGenerative(generator model.Generator) *Generative {
	return &Generative{generator: generator}
}

func (reconciler *Generative) Reconcile(ctx context.Context, request Request) ([]Action, error) {
	if len(request.New) == 0 || len(request.Existing) == 0 {
		return nil, nil
	}
	prompt := reconciliationPrompt(request)
	response, err := reconciler.generator.Generate(ctx, prompt)
	if err != nil {
		return nil, err
	}
	actions, err := parseActions(response, request)
	if err == nil {
		return actions, nil
	}
	response, generateErr := reconciler.generator.Generate(ctx, "Return one valid JSON object only. Do not add markdown or explanations.\n\n"+prompt)
	if generateErr != nil {
		return nil, generateErr
	}
	actions, err = parseActions(response, request)
	if err != nil {
		return nil, fmt.Errorf("reconciliation model response invalid after retry: %w", err)
	}
	return actions, nil
}

func reconciliationPrompt(request Request) string {
	return fmt.Sprintf(`Reconcile newly extracted durable knowledge with active knowledge from earlier coding-assistant sessions. Return only a JSON object with one actions array. Each action must identify one new_source_id, one existing_source_id, and relation "duplicate" or "supersedes".

Use "duplicate" only when both records express the same durable claim. Use "supersedes" only when the new record gives a confirmed, incompatible newer state for the same subject and attribute. Do not emit actions for complementary knowledge, merely related subjects, uncertain conflicts, or independent problem occurrences. The application validates chronology, scope, project, kind, evidence, and acceptance separately. Prefer no action when uncertain. Never relate records with different kinds, scopes, or projects.

Project: %s
New records:
%s

Existing records:
%s

Return the JSON object now.`, request.Project, formatDocuments(request.New), formatDocuments(request.Existing))
}

func formatDocuments(documents []knowledge.Document) string {
	var formatted strings.Builder
	for _, document := range documents {
		fmt.Fprintf(&formatted, "ID: %s\nTIMESTAMP: %s\nKIND: %s\nSCOPE: %s\nPROJECT: %s\nTITLE: %s\nCONTENT:\n%s\n\n",
			document.SourceID, document.TS, document.Kind, document.Scope, document.Project, document.Title, document.Content)
	}
	return strings.TrimSpace(formatted.String())
}

func parseActions(response string, request Request) ([]Action, error) {
	var payload struct {
		Actions []struct {
			NewSourceID      string   `json:"new_source_id"`
			ExistingSourceID string   `json:"existing_source_id"`
			Relation         Relation `json:"relation"`
		} `json:"actions"`
	}
	response = strings.TrimSpace(response)
	if strings.HasPrefix(response, "```") {
		return nil, errors.New("markdown fence is not allowed")
	}
	if err := json.Unmarshal([]byte(response), &payload); err != nil {
		return nil, err
	}
	newByID := documentsByID(request.New)
	existingByID := documentsByID(request.Existing)
	seen := make(map[string]bool)
	actions := make([]Action, 0, len(payload.Actions))
	for _, raw := range payload.Actions {
		newDocument, newExists := newByID[raw.NewSourceID]
		existingDocument, existingExists := existingByID[raw.ExistingSourceID]
		if !newExists || !existingExists {
			return nil, errors.New("action references an unknown record")
		}
		if raw.Relation != RelationDuplicate && raw.Relation != RelationSupersedes {
			return nil, fmt.Errorf("invalid relation %q", raw.Relation)
		}
		if newDocument.Kind == "problem" || !compatible(newDocument, existingDocument) {
			return nil, errors.New("action relates incompatible records")
		}
		key := raw.NewSourceID + "\n" + raw.ExistingSourceID
		if seen[key] {
			continue
		}
		seen[key] = true
		actions = append(actions, Action{NewSourceID: raw.NewSourceID, ExistingSourceID: raw.ExistingSourceID, Relation: raw.Relation})
	}
	return actions, nil
}

func documentsByID(documents []knowledge.Document) map[string]knowledge.Document {
	result := make(map[string]knowledge.Document, len(documents))
	for _, document := range documents {
		result[document.SourceID] = document
	}
	return result
}

func compatible(first, second knowledge.Document) bool {
	return first.Kind == second.Kind && first.Scope == second.Scope && first.Project == second.Project
}
