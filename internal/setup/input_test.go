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
	if result.Embedding.URI != appconfig.DefaultEmbeddingURI || len(result.Integrations) != 2 || result.Generation != nil {
		t.Fatalf("defaults were not preserved: %#v", result)
	}
}

func TestSetupSelectsAndRetainsLocalGeneration(t *testing.T) {
	var output bytes.Buffer
	result, err := askConfiguration(strings.NewReader("\n\nlocal\n\n"), &output, appconfig.Default())
	if err != nil {
		t.Fatal(err)
	}
	if result.Generation == nil || *result.Generation != appconfig.DefaultGenerationConfig() {
		t.Fatalf("default local model = %#v", result.Generation)
	}
	if !strings.Contains(output.String(), appconfig.DefaultGenerationModel) {
		t.Fatal("setup did not suggest the Qwen3 model")
	}
	custom, err := askConfiguration(strings.NewReader("\n\n\nhf://example/model/custom.gguf\n"), &output, result)
	if err != nil || custom.Generation == nil || custom.Generation.Name() != "custom.gguf" {
		t.Fatalf("custom local model = %#v, error = %v", custom.Generation, err)
	}
	harness, err := askConfiguration(strings.NewReader("\n\nharness\n"), &output, custom)
	if err != nil || harness.Generation != nil {
		t.Fatalf("harness selection = %#v, error = %v", harness.Generation, err)
	}
}
