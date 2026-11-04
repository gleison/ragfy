package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentGeneratorsUseIsolatedNonInteractiveModes(t *testing.T) {
	codex := NewCodexGenerator("codex", "/tmp/empty", "/tmp/schema.json")
	for _, want := range []string{"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules", "hooks", "read-only", "/tmp/schema.json"} {
		if !containsArgument(codex.arguments, want) {
			t.Fatalf("codex arguments missing %q: %#v", want, codex.arguments)
		}
	}
	claude := NewClaudeGenerator("claude", "/tmp/empty", `{"type":"object"}`)
	for _, want := range []string{"-p", "--safe-mode", "--restricted", "--no-session-persistence", "--json-schema"} {
		if !containsArgument(claude.arguments, want) {
			t.Fatalf("claude arguments missing %q: %#v", want, claude.arguments)
		}
	}
	toolsIndex := argumentIndex(claude.arguments, "--tools")
	if toolsIndex < 0 || toolsIndex+1 >= len(claude.arguments) || claude.arguments[toolsIndex+1] != "" {
		t.Fatalf("claude tools were not disabled: %#v", claude.arguments)
	}
}

func TestAgentOutputParsers(t *testing.T) {
	structured := `{"problems":[],"facts":[],"preferences":[],"decisions":[]}`
	if got, err := parseCodexOutput([]byte(" \n" + structured + "\n")); err != nil || got != structured {
		t.Fatalf("codex output = %q, err=%v", got, err)
	}
	envelope, _ := json.Marshal(map[string]any{"is_error": false, "result": "ignored", "structured_output": json.RawMessage(structured)})
	got, err := parseClaudeOutput(envelope)
	if err != nil || got != structured {
		t.Fatalf("claude output = %q, err=%v", got, err)
	}
	if _, err := parseClaudeOutput([]byte(`{"result":"","structured_output":null}`)); err == nil {
		t.Fatal("claude parser accepted an empty structured result")
	}
}

func TestLimitedBufferCapsOutputWithoutBreakingWriter(t *testing.T) {
	buffer := newLimitedBuffer(4)
	if written, err := buffer.Write([]byte("abcdef")); err != nil || written != 6 {
		t.Fatalf("write = %d, err=%v", written, err)
	}
	if buffer.String() != "abcd" || !buffer.Exceeded() {
		t.Fatalf("unexpected limited buffer: %q exceeded=%v", buffer.String(), buffer.Exceeded())
	}
}

func containsArgument(arguments []string, target string) bool {
	return argumentIndex(arguments, target) >= 0
}

func argumentIndex(arguments []string, target string) int {
	for index, argument := range arguments {
		if strings.TrimSpace(argument) == target {
			return index
		}
	}
	return -1
}
