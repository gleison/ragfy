package hook

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordingHTTPClient struct {
	payload       map[string]any
	authorization string
	path          string
	response      string
}

func (client *recordingHTTPClient) Do(request *http.Request) (*http.Response, error) {
	client.authorization = request.Header.Get("Authorization")
	client.path = request.URL.Path
	client.payload = make(map[string]any)
	if err := json.NewDecoder(request.Body).Decode(&client.payload); err != nil {
		return nil, err
	}
	response := client.response
	if response == "" {
		response = `{}`
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
}

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

func TestRunAddsExplicitProviderSource(t *testing.T) {
	dataDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDirectory, "token"), []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client := &recordingHTTPClient{}
	input := strings.NewReader(`{"session_id":"s1","transcript_path":"/tmp/session.jsonl"}`)
	run("digest", "claude", dataDirectory, "127.0.0.1:37841", input, &bytes.Buffer{}, client)
	if client.authorization != "Bearer secret" {
		t.Fatalf("authorization = %q", client.authorization)
	}
	if client.payload["source"] != "claude" || client.payload["session_id"] != "s1" {
		t.Fatalf("received payload = %#v", client.payload)
	}
}

func TestRunWritesSessionStartContext(t *testing.T) {
	dataDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDirectory, "token"), []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client := &recordingHTTPClient{response: `{"context":"project context"}`}
	var output bytes.Buffer
	run("bootstrap", "codex", dataDirectory, "127.0.0.1:37841", strings.NewReader(`{"session_id":"s1","cwd":"/workspace/ragfy"}`), &output, client)
	if client.path != "/bootstrap" {
		t.Fatalf("request path = %q", client.path)
	}
	var envelope struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.HookSpecificOutput.HookEventName != "SessionStart" || envelope.HookSpecificOutput.AdditionalContext != "project context" {
		t.Fatalf("bootstrap output = %#v", envelope)
	}
}

func TestRunResetsContextBeforeCompaction(t *testing.T) {
	dataDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDirectory, "token"), []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client := &recordingHTTPClient{response: `{"reset":true}`}
	var output bytes.Buffer
	run("reset", "codex", dataDirectory, "127.0.0.1:37841", strings.NewReader(`{"session_id":"s1"}`), &output, client)
	if client.path != "/context/reset" {
		t.Fatalf("request path = %q", client.path)
	}
	if output.Len() != 0 {
		t.Fatalf("reset hook output = %q", output.String())
	}
}

func TestRunSkipsNestedAgentExecution(t *testing.T) {
	t.Setenv("RAGFY_AGENT_EXEC", "1")
	Run("digest", "codex", t.TempDir(), "127.0.0.1:1", strings.NewReader(`{"session_id":"nested"}`), &bytes.Buffer{})
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
