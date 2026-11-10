package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ragfy/internal/knowledge"
)

func TestResolveIngestPathUsesCallerWorkingDirectory(t *testing.T) {
	workingDirectory := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDirectory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveIngestPath("docs")
	if err != nil || resolved != filepath.Join(current, "docs") {
		t.Fatalf("resolved path = %q, %v", resolved, err)
	}
}

func TestCommandArgumentsHandlesDefaultStatusInvocation(t *testing.T) {
	if arguments := commandArguments([]string{"ragfy"}); len(arguments) != 0 {
		t.Fatalf("default arguments = %#v", arguments)
	}
	arguments := commandArguments([]string{"ragfy", "status", "--json"})
	if len(arguments) != 1 || arguments[0] != "--json" {
		t.Fatalf("status arguments = %#v", arguments)
	}
}

func TestHelpDescribesSearchAndTextIngest(t *testing.T) {
	var global bytes.Buffer
	writeGlobalHelp(&global)
	for _, expected := range []string{"Find stored memories", "Extract durable knowledge", "ragfy help <command>"} {
		if !strings.Contains(global.String(), expected) {
			t.Fatalf("global help missing %q: %s", expected, global.String())
		}
	}
	var ingest bytes.Buffer
	if !writeCommandHelp(&ingest, "ingest") || !strings.Contains(ingest.String(), "--text") || !strings.Contains(ingest.String(), "same pipeline as session digest") || !strings.Contains(ingest.String(), "Provider selection is automatic") {
		t.Fatalf("ingest help = %q", ingest.String())
	}
}

func TestHelpDescribesSessionAndRecallAudit(t *testing.T) {
	var global bytes.Buffer
	writeGlobalHelp(&global)
	if !strings.Contains(global.String(), "List, inspect, or forget") || !strings.Contains(global.String(), "memories injected") {
		t.Fatalf("global help = %q", global.String())
	}
	var sessions, recalls bytes.Buffer
	if !writeCommandHelp(&sessions, "sessions") || !strings.Contains(sessions.String(), "sessions forget") || !strings.Contains(sessions.String(), "reactivates") {
		t.Fatalf("sessions help = %q", sessions.String())
	}
	if !writeCommandHelp(&recalls, "recalls") || !strings.Contains(recalls.String(), "ranking scores") {
		t.Fatalf("recalls help = %q", recalls.String())
	}
}

func TestHelpDescribesSetupHooksAndSearchProjectDefault(t *testing.T) {
	var global, setupHooks, search bytes.Buffer
	writeGlobalHelp(&global)
	if !strings.Contains(global.String(), "ragfy setup hooks") {
		t.Fatalf("global help = %q", global.String())
	}
	if !writeCommandHelp(&setupHooks, "setup hooks") || !strings.Contains(setupHooks.String(), "unrelated hooks are preserved") {
		t.Fatalf("setup hooks help = %q", setupHooks.String())
	}
	if !writeCommandHelp(&search, "search") || !strings.Contains(search.String(), "current directory name") || !strings.Contains(search.String(), "overrides") {
		t.Fatalf("search help = %q", search.String())
	}
}

func TestSetupHooksHelpUsesNestedCommandWithoutApplyingHooks(t *testing.T) {
	for _, invoke := range []func(*bytes.Buffer) error{
		func(output *bytes.Buffer) error { return setupCommand([]string{"hooks", "--help"}, output) },
		func(output *bytes.Buffer) error { return helpCommand([]string{"setup", "hooks"}, output) },
	} {
		var output bytes.Buffer
		if err := invoke(&output); err != nil || !strings.Contains(output.String(), "Usage: ragfy setup hooks") {
			t.Fatalf("setup hooks help = %q, %v", output.String(), err)
		}
	}
}

func TestJSONCommandArgumentsAcceptsFlagBeforeOrAfterIdentifier(t *testing.T) {
	for _, arguments := range [][]string{{"--json", "session"}, {"session", "--json"}} {
		remaining, asJSON, err := jsonCommandArguments(arguments)
		if err != nil || !asJSON || len(remaining) != 1 || remaining[0] != "session" {
			t.Fatalf("arguments=%#v remaining=%#v json=%v err=%v", arguments, remaining, asJSON, err)
		}
	}
}

func TestRecallShowFocusesOnInjectedMemoriesWhileExplainShowsAuditDetails(t *testing.T) {
	event := knowledge.RecallEvent{
		ID:             "recall-1",
		SessionID:      "session-1",
		Source:         "codex",
		Project:        "ragfy",
		Prompt:         "How does recall work?",
		CreatedAt:      "2026-11-10T12:00:00Z",
		ResultCount:    1,
		CandidateCount: 2,
		ScoreCutoff:    0.7,
		LexicalCutoff:  0.5,
		Results: []knowledge.RecallResult{
			{
				Rank: 3, SelectedRank: 1, Status: "selected", Feedback: "useful",
				SourceID: "selected-id", Title: "Selected memory", Content: "selected content", Kind: "fact",
				Score: 0.9, SemanticScore: 0.8, LexicalScore: 0.7, ProjectBoost: 0.1,
			},
			{
				Rank: 4, Status: "cutoff", SourceID: "cut-id", Title: "Cut candidate", Content: "cut content", Kind: "decision",
				Score: 0.4, SemanticScore: 0.3, LexicalScore: 0.2,
			},
		},
	}

	var show bytes.Buffer
	writeRecallShow(&show, event)
	for _, expected := range []string{"Injected memories:", "[candidate 3, injected 1] Selected memory", "feedback=useful", "selected content"} {
		if !strings.Contains(show.String(), expected) {
			t.Fatalf("show missing %q: %s", expected, show.String())
		}
	}
	for _, internal := range []string{"Candidates:", "score=", "status=selected", "selected-id", "cut content"} {
		if strings.Contains(show.String(), internal) {
			t.Fatalf("show contains audit detail %q: %s", internal, show.String())
		}
	}

	var explain bytes.Buffer
	writeRecallExplanation(&explain, event)
	for _, expected := range []string{
		"Candidates: 2; selected: 1",
		"selected-id",
		"status=selected selected_rank=1 feedback=useful",
		"score=0.9000",
		"status=cutoff",
		"cut content",
	} {
		if !strings.Contains(explain.String(), expected) {
			t.Fatalf("explain missing %q: %s", expected, explain.String())
		}
	}
}
