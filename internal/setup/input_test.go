package setup

import (
	"bytes"
	"strings"
	"testing"

	appconfig "ragfy/internal/config"
)

func TestSetupAcceptsCustomModelAndCodexOnly(t *testing.T) {
	input := strings.NewReader("https://example.com/embedding.gguf\n1\n")
	var output bytes.Buffer
	result, err := askConfiguration(input, &output, appconfig.Default())
	if err != nil {
		t.Fatal(err)
	}
	if result.Embedding.URI != "https://example.com/embedding.gguf" || result.Embedding.Pooling != "auto" || result.Embedding.QueryInstruction != "" {
		t.Fatalf("custom embedding = %#v", result.Embedding)
	}
	if len(result.Integrations) != 1 || result.Integrations[0] != "codex" {
		t.Fatalf("integrations = %#v", result.Integrations)
	}
	if !strings.Contains(output.String(), "[x] 1. Codex") || !strings.Contains(output.String(), "[x] 2. Claude Code") {
		t.Fatalf("setup did not render checkboxes: %q", output.String())
	}
}

func TestSetupKeepsDefaultsOnBlankInput(t *testing.T) {
	result, err := askConfiguration(strings.NewReader("\n\n"), &bytes.Buffer{}, appconfig.Default())
	if err != nil {
		t.Fatal(err)
	}
	if result.Embedding.URI != appconfig.DefaultEmbeddingURI || len(result.Integrations) != 2 {
		t.Fatalf("defaults were not preserved: %#v", result)
	}
}
