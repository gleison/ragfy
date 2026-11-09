package config

import (
	"path/filepath"
	"testing"
)

func TestSaveAndLoadCustomConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	configuration := Config{
		Version:      CurrentVersion,
		Embedding:    EmbeddingConfig{URI: "https://example.com/model.gguf", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Pooling: "auto"},
		Integrations: []string{"codex"},
	}
	if err := Save(path, configuration); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Embedding.URI != configuration.Embedding.URI || !loaded.Enabled("codex") || loaded.Enabled("claude") {
		t.Fatalf("loaded configuration = %#v", loaded)
	}
}

func TestConfigurationValidationRejectsInvalidModelsAndSelections(t *testing.T) {
	tests := []Config{
		{Version: CurrentVersion, Embedding: CustomEmbeddingConfig("https://example.com/model.bin"), Integrations: []string{"codex"}},
		{Version: CurrentVersion, Embedding: DefaultEmbeddingConfig(), Integrations: nil},
		{Version: CurrentVersion, Embedding: DefaultEmbeddingConfig(), Integrations: []string{"codex", "claude", "codex"}},
	}
	for _, test := range tests {
		if err := test.Validate(); err == nil {
			t.Fatalf("invalid configuration accepted: %#v", test)
		}
	}
}
