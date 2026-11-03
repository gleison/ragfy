package hook

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestWritePromptHookOutputUsesCodexJSONEnvelope(t *testing.T) {
	context := "[1] Retrieved knowledge\nDetails"
	var output bytes.Buffer
	if err := WritePromptOutput(&output, context); err != nil {
		t.Fatal(err)
	}

	var envelope struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatalf("hook output is not valid JSON: %v; output=%q", err, output.String())
	}
	if envelope.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Fatalf("hookEventName = %q", envelope.HookSpecificOutput.HookEventName)
	}
	if envelope.HookSpecificOutput.AdditionalContext != context {
		t.Fatalf("additionalContext = %q, want %q", envelope.HookSpecificOutput.AdditionalContext, context)
	}
}

func TestWritePromptHookOutputIsSilentWithoutContext(t *testing.T) {
	var output bytes.Buffer
	if err := WritePromptOutput(&output, "  \n"); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("empty context produced output %q", output.String())
	}
}
