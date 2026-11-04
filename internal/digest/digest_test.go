package digest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ragfy/internal/knowledge"
)

func TestJSONSchemaIsValid(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(JSONSchema), &schema); err != nil {
		t.Fatalf("invalid digest JSON schema: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("unexpected schema root: %#v", schema)
	}
}

func TestParseRecordsRejectsInvalidExtraction(t *testing.T) {
	spine := "USER: Ragfy runs as a persistent local Go daemon."
	tests := map[string]string{
		"schema placeholder": `{"problems":[],"facts":[{"fact":"durable self-contained state","area":"runtime","status":"current","evidence":"Ragfy runs as a persistent local Go daemon.","visibility":"project"}],"preferences":[],"decisions":[]}`,
		"truncated JSON":     `{"problems":[],"facts":[{"fact":"Ragfy runs as a local Go daemon."}],`,
		"missing arrays":     `{"facts":[]}`,
	}
	for name, output := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRecords(output, spine); err == nil {
				t.Fatalf("parseRecords accepted invalid output: %s", output)
			}
		})
	}
}

func TestParseRecordsAcceptsGroundedKnowledge(t *testing.T) {
	spine := "USER: Ragfy runs as a persistent local Go daemon."
	output := `{"problems":[],"facts":[{"fact":"Ragfy runs as a persistent local Go daemon.","area":"ragfy runtime","status":"current","evidence":"Ragfy runs as a persistent local Go daemon.","visibility":"project"}],"preferences":[],"decisions":[]}`
	docs, err := ParseRecords(output, spine)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
	if docs[0].Kind != "fact" || !strings.Contains(docs[0].Content, "persistent local Go daemon") {
		t.Fatalf("unexpected document: %#v", docs[0])
	}
	if docs[0].Scope != knowledge.ScopeProject {
		t.Fatalf("scope = %q", docs[0].Scope)
	}
	if docs[0].Metadata["evidence"] != "Ragfy runs as a persistent local Go daemon." {
		t.Fatalf("evidence was not preserved: %#v", docs[0].Metadata)
	}
}

func TestParseRecordsRejectsUngroundedAndTransientKnowledge(t *testing.T) {
	tests := []struct {
		name   string
		spine  string
		output string
	}{
		{
			name:   "fabricated evidence",
			spine:  "USER: Ragfy uses SQLite.",
			output: `{"problems":[],"facts":[{"fact":"Ragfy uses PostgreSQL.","area":"storage","status":"current","evidence":"Ragfy uses PostgreSQL.","visibility":"project"}],"preferences":[],"decisions":[]}`,
		},
		{
			name:   "transient fact",
			spine:  "CODEX: The current hash differs from the stored hash.",
			output: `{"problems":[],"facts":[{"fact":"The current hash differs from the stored hash.","area":"current session","status":"current","evidence":"The current hash differs from the stored hash.","visibility":"project"}],"preferences":[],"decisions":[]}`,
		},
		{
			name:   "assistant preference",
			spine:  "CODEX: Prefer SQLite for the MVP.",
			output: `{"problems":[],"facts":[],"preferences":[{"preference":"Prefer SQLite for the MVP.","why":"","applies_to":"storage","evidence":"Prefer SQLite for the MVP.","visibility":"project"}],"decisions":[]}`,
		},
		{
			name:   "assistant decision",
			spine:  "CODEX: Increase the hook timeout to avoid transient failures.",
			output: `{"problems":[],"facts":[],"preferences":[],"decisions":[{"decision":"Increase the hook timeout to avoid transient failures.","rejected":"","why":"","evidence":"Increase the hook timeout to avoid transient failures.","visibility":"project"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs, err := ParseRecords(tt.output, tt.spine)
			if err != nil {
				t.Fatal(err)
			}
			if len(docs) != 0 {
				t.Fatalf("got unexpected documents: %#v", docs)
			}
		})
	}
}

func TestParseRecordsAuditExplainsAcceptedAndRejectedCandidates(t *testing.T) {
	spine := "USER: Ragfy uses SQLite for durable local storage.\n\nCODEX: Prefer SQLite for every project."
	output := `{"problems":[],"facts":[{"fact":"Ragfy uses SQLite for durable local storage.","area":"storage","status":"current","evidence":"Ragfy uses SQLite for durable local storage.","visibility":"project"},{"fact":"The current hash differs from the stored hash.","area":"session","status":"current","evidence":"The current hash differs from the stored hash.","visibility":"project"}],"preferences":[{"preference":"Prefer SQLite for every project.","why":"","applies_to":"storage","evidence":"Prefer SQLite for every project.","visibility":"global"}],"decisions":[]}`
	docs, audit, err := ParseRecordsWithAudit(output, spine)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
	want := "candidates=3 accepted=1 rejected=2 candidates_by_kind=fact:2,preference:1 accepted_by_kind=fact:1 accepted_by_scope=project:1 rejection_reasons=evidence_not_from_user:1,transient_fact:1"
	if got := audit.Summary(); got != want {
		t.Fatalf("audit summary = %q, want %q", got, want)
	}
}

func TestParseRecordsAuditDistinguishesEmptyModelResponse(t *testing.T) {
	_, audit, err := ParseRecordsWithAudit(`{"problems":[],"facts":[],"preferences":[],"decisions":[]}`, "USER: General discussion with no durable knowledge.")
	if err != nil {
		t.Fatal(err)
	}
	want := "candidates=0 accepted=0 rejected=0 candidates_by_kind=none accepted_by_kind=none accepted_by_scope=none rejection_reasons=none"
	if got := audit.Summary(); got != want {
		t.Fatalf("audit summary = %q, want %q", got, want)
	}
}

func TestParseRecordsAcceptsImplicitlyAcceptedDecision(t *testing.T) {
	claim := "Use codex exec in read-only mode for Codex session extraction."
	acceptance := "That is our next task; implement the proposed extractor."
	spine := "CODEX: " + claim + "\n\nUSER: " + acceptance
	output := fmt.Sprintf(`{"problems":[],"facts":[],"preferences":[],"decisions":[{"decision":"Use codex exec in read-only mode for Codex sessions.","rejected":"Local generation","why":"Use the session provider","evidence":%q,"acceptance":"implicit","acceptance_evidence":%q,"visibility":"project"}]}`, claim, acceptance)
	docs, audit, err := ParseRecordsWithAudit(output, spine)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Kind != "decision" {
		t.Fatalf("unexpected documents: %#v", docs)
	}
	if docs[0].Metadata["acceptance_evidence"] != acceptance {
		t.Fatalf("acceptance evidence was not preserved: %#v", docs[0].Metadata)
	}
	if audit.Accepted["decision"] != 1 {
		t.Fatalf("unexpected audit: %s", audit.Summary())
	}
}

func TestParseRecordsRejectsImplicitDecisionWithoutLaterUserAction(t *testing.T) {
	claim := "Use codex exec in read-only mode for Codex session extraction."
	spine := "USER: Earlier context.\n\nCODEX: " + claim
	output := fmt.Sprintf(`{"problems":[],"facts":[],"preferences":[],"decisions":[{"decision":"Use codex exec in read-only mode for Codex sessions.","rejected":"","why":"","evidence":%q,"acceptance":"implicit","acceptance_evidence":"Earlier context.","visibility":"project"}]}`, claim)
	docs, audit, err := ParseRecordsWithAudit(output, spine)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 || audit.Rejected["implicit_acceptance_not_from_later_user"] != 1 {
		t.Fatalf("unexpected result: docs=%#v audit=%s", docs, audit.Summary())
	}
}

func TestPreferenceEvidenceCanComeFromLaterUserParagraph(t *testing.T) {
	spine := "USER: Some context.\n\nAlways keep the README in English.\n\nCODEX: Understood."
	output := `{"problems":[],"facts":[],"preferences":[{"preference":"Keep project documentation in English.","why":"","applies_to":"README and context","evidence":"Keep the README in English.","visibility":"global"}],"decisions":[]}`
	output = strings.Replace(output, "Keep the README in English.", "Always keep the README in English.", 1)
	docs, err := ParseRecords(output, spine)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Kind != "preference" {
		t.Fatalf("unexpected documents: %#v", docs)
	}
}

func TestParseRecordsAcceptsUserPreferenceAndRejectsDuplicateEvidence(t *testing.T) {
	spine := "USER: The service reloads the model on every hook call.\n\nUSER: The extracted knowledge is not refined enough."
	output := `{"problems":[{"problem":"The service reloads the model on every hook call.","context":"hooks","solution":"Keep one persistent local service running.","root_cause":"Each hook starts the model runtime again.","evidence":"The service reloads the model on every hook call.","visibility":"project"}],"facts":[],"preferences":[{"preference":"Use refined documentation conventions.","why":"","applies_to":"documentation","evidence":"The extracted knowledge is not refined enough.","visibility":"global"}],"decisions":[{"decision":"Keep one persistent local service running.","rejected":"Reload per hook","why":"Avoid repeated startup","evidence":"The service reloads the model on every hook call.","visibility":"project"}]}`
	docs, err := ParseRecords(output, spine)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Kind != "problem" || docs[1].Kind != "preference" {
		t.Fatalf("expected grounded problem and user preference, got %#v", docs)
	}
}

func TestTranscriptSpineKeepsFullConversationAndExcludesInjectedContent(t *testing.T) {
	data := []byte(strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Architecture at the beginning must remain visible."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"durable self-contained state and standing instruction"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"<environment_context>injected runtime data</environment_context>"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"` + strings.Repeat("middle content ", 40) + `"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"The final confirmed behavior must remain visible."}}`,
	}, "\n"))
	spine := TranscriptSpine(data)
	if strings.Contains(spine, "durable self-contained state") {
		t.Fatalf("contaminated example remained in spine: %s", spine)
	}
	if strings.Contains(spine, "injected runtime data") {
		t.Fatalf("injected environment context remained in spine: %s", spine)
	}
	for _, want := range []string{"Architecture at the beginning", "middle content", "final confirmed behavior"} {
		if !strings.Contains(spine, want) {
			t.Fatalf("full spine lost %q: %s", want, spine)
		}
	}
}

func TestTranscriptSpineSupportsCodexAndClaudeMessageFormats(t *testing.T) {
	data := []byte(strings.Join([]string{
		`{"type":"user","message":{"content":"Claude user message"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Claude assistant message"},{"type":"tool_use","text":"ignored tool"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Codex response item"}]}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Codex event message"}}`,
		`{"type":"response_item","payload":{"type":"function_call","role":"assistant","content":"ignored call"}}`,
		`not-json`,
	}, "\n"))
	spine := TranscriptSpine(data)
	for _, want := range []string{
		"USER: Claude user message",
		"CLAUDE: Claude assistant message",
		"USER: Codex response item",
		"CODEX: Codex event message",
	} {
		if !strings.Contains(spine, want) {
			t.Fatalf("missing %q in spine:\n%s", want, spine)
		}
	}
	if strings.Contains(spine, "ignored tool") || strings.Contains(spine, "ignored call") {
		t.Fatalf("non-message content leaked into spine:\n%s", spine)
	}
}

func TestTranscriptWindowsOverlapCompleteMessages(t *testing.T) {
	spine := strings.Join([]string{
		"USER: First message has context.",
		"CODEX: Second message adds detail.",
		"CODEX: Use modular Go interfaces.",
		"USER: De acordo. Pode seguir.",
		"CODEX: Final implementation notes.",
	}, "\n\n")
	windows := TranscriptWindows(spine, 105, 35)
	if len(windows) < 2 {
		t.Fatalf("expected multiple windows, got %#v", windows)
	}
	pair := "CODEX: Use modular Go interfaces.\n\nUSER: De acordo. Pode seguir."
	foundPair := false
	for _, window := range windows {
		if strings.Contains(window, pair) {
			foundPair = true
		}
		for _, block := range strings.Split(window, "\n\n") {
			if !strings.HasPrefix(block, "USER: ") && !strings.HasPrefix(block, "CODEX: ") && !strings.HasPrefix(block, "CLAUDE: ") {
				t.Fatalf("window split a message: %q", block)
			}
		}
	}
	if !foundPair {
		t.Fatalf("proposal and acceptance were separated: %#v", windows)
	}
}

func TestExplicitPreferenceAcceptsNaturalPortugueseWording(t *testing.T) {
	evidence := "O código em si deve ser sua própria documentação, com métodos atômicos e nomes significativos."
	spine := "USER: " + evidence
	output := fmt.Sprintf(`{"problems":[],"facts":[],"preferences":[{"preference":"Preferir código autodocumentado com métodos atômicos e nomes significativos.","why":"","applies_to":"código Go","evidence":%q,"acceptance":"explicit","acceptance_evidence":"","visibility":"global"}],"decisions":[]}`, evidence)
	documents, audit, err := ParseRecordsWithAudit(output, spine)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Kind != "preference" || audit.Rejected["preference_not_explicit"] != 0 {
		t.Fatalf("documents=%#v audit=%s", documents, audit.Summary())
	}
}

func TestTranscriptTextAcceptsSupportedTextBlocksOnly(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "plain"},
		map[string]any{"type": "input_text", "text": "input"},
		map[string]any{"type": "output_text", "text": "output"},
		map[string]any{"type": "tool_result", "text": "ignored"},
		"invalid block",
	}
	if got := transcriptText(content); got != "plain\ninput\noutput" {
		t.Fatalf("transcriptText = %q", got)
	}
	if got := transcriptText(map[string]any{"text": "unsupported"}); got != "" {
		t.Fatalf("unsupported content = %q", got)
	}
}

func TestKnowledgeOverlapDetectsProblemDecisionDuplication(t *testing.T) {
	problem := "Modelo recarrega em cada chamada do hook. Solução: migrar para um serviço permanente."
	decision := "Migrar para arquitetura de serviço permanente porque o modelo recarrega a cada chamada do hook."
	if overlap := knowledgeOverlap(problem, decision); overlap < 0.35 {
		t.Fatalf("overlap = %.2f, want at least 0.35", overlap)
	}
	if overlap := knowledgeOverlap(problem, "Manter o README e o Context em inglês."); overlap >= 0.35 {
		t.Fatalf("unrelated overlap = %.2f, want less than 0.35", overlap)
	}
}
