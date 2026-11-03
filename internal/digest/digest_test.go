package digest

import (
	"strings"
	"testing"
)

func TestParseRecordsRejectsInvalidExtraction(t *testing.T) {
	spine := "USER: Ragfy runs as a persistent local Go daemon."
	tests := map[string]string{
		"schema placeholder": `{"problems":[],"facts":[{"fact":"durable self-contained state","scope":"area","status":"current","evidence":"Ragfy runs as a persistent local Go daemon."}],"preferences":[],"decisions":[]}`,
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
	output := `{"problems":[],"facts":[{"fact":"Ragfy runs as a persistent local Go daemon.","scope":"ragfy runtime","status":"current","evidence":"Ragfy runs as a persistent local Go daemon."}],"preferences":[],"decisions":[]}`
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
			output: `{"problems":[],"facts":[{"fact":"Ragfy uses PostgreSQL.","scope":"storage","status":"current","evidence":"Ragfy uses PostgreSQL."}],"preferences":[],"decisions":[]}`,
		},
		{
			name:   "transient fact",
			spine:  "CODEX: The current hash differs from the stored hash.",
			output: `{"problems":[],"facts":[{"fact":"The current hash differs from the stored hash.","scope":"current session","status":"current","evidence":"The current hash differs from the stored hash."}],"preferences":[],"decisions":[]}`,
		},
		{
			name:   "assistant preference",
			spine:  "CODEX: Prefer SQLite for the MVP.",
			output: `{"problems":[],"facts":[],"preferences":[{"preference":"Prefer SQLite for the MVP.","why":"","applies_to":"storage","evidence":"Prefer SQLite for the MVP."}],"decisions":[]}`,
		},
		{
			name:   "assistant decision",
			spine:  "CODEX: Increase the hook timeout to avoid transient failures.",
			output: `{"problems":[],"facts":[],"preferences":[],"decisions":[{"decision":"Increase the hook timeout to avoid transient failures.","rejected":"","why":"","evidence":"Increase the hook timeout to avoid transient failures."}]}`,
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

func TestPreferenceEvidenceCanComeFromLaterUserParagraph(t *testing.T) {
	spine := "USER: Some context.\n\nAlways keep the README in English.\n\nCODEX: Understood."
	output := `{"problems":[],"facts":[],"preferences":[{"preference":"Keep project documentation in English.","why":"","applies_to":"README and context","evidence":"Keep the README in English."}],"decisions":[]}`
	output = strings.Replace(output, "Keep the README in English.", "Always keep the README in English.", 1)
	docs, err := ParseRecords(output, spine)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Kind != "preference" {
		t.Fatalf("unexpected documents: %#v", docs)
	}
}

func TestParseRecordsRejectsUnrelatedPreferenceAndDuplicateEvidence(t *testing.T) {
	spine := "USER: The service reloads the model on every hook call.\n\nUSER: The extracted knowledge is not refined enough."
	output := `{"problems":[{"problem":"The service reloads the model on every hook call.","context":"hooks","solution":"Keep one persistent local service running.","root_cause":"Each hook starts the model runtime again.","evidence":"The service reloads the model on every hook call."}],"facts":[],"preferences":[{"preference":"Use refined documentation conventions.","why":"","applies_to":"documentation","evidence":"The extracted knowledge is not refined enough."}],"decisions":[{"decision":"Keep one persistent local service running.","rejected":"Reload per hook","why":"Avoid repeated startup","evidence":"The service reloads the model on every hook call."}]}`
	docs, err := ParseRecords(output, spine)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Kind != "problem" {
		t.Fatalf("expected only the first grounded record, got %#v", docs)
	}
}

func TestTranscriptSpineExcludesContaminatedExamplesAndBalancesLongText(t *testing.T) {
	data := []byte(strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Architecture at the beginning must remain visible."}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"durable self-contained state and standing instruction"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"` + strings.Repeat("middle content ", 40) + `"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"The final confirmed behavior must remain visible."}}`,
	}, "\n"))
	spine := TranscriptSpine(data, 300)
	if strings.Contains(spine, "durable self-contained state") {
		t.Fatalf("contaminated example remained in spine: %s", spine)
	}
	if !strings.Contains(spine, "Architecture at the beginning") || !strings.Contains(spine, "final confirmed behavior") {
		t.Fatalf("balanced excerpt lost beginning or end: %s", spine)
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
	spine := TranscriptSpine(data, 5000)
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
