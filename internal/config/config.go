package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

const (
	CurrentVersion          = 1
	DefaultEmbeddingURI     = "hf://Qwen/Qwen3-Embedding-0.6B-GGUF/Qwen3-Embedding-0.6B-Q8_0.gguf"
	DefaultEmbeddingSHA256  = "06507c7b42688469c4e7298b0a1e16deff06caf291cf0a5b278c308249c3e439"
	DefaultEmbeddingPooling = "last"
	DefaultQueryInstruction = "Instruct: Given a coding assistant prompt, retrieve relevant passages from the user's knowledge base that help answer the prompt.\nQuery: "
)

type EmbeddingConfig struct {
	URI              string `json:"uri"`
	SHA256           string `json:"sha256,omitempty"`
	Pooling          string `json:"pooling,omitempty"`
	QueryInstruction string `json:"query_instruction,omitempty"`
}

type Config struct {
	Version      int             `json:"version"`
	Embedding    EmbeddingConfig `json:"embedding"`
	Integrations []string        `json:"integrations"`
}

func Default() Config {
	return Config{Version: CurrentVersion, Embedding: DefaultEmbeddingConfig(), Integrations: []string{"codex", "claude"}}
}

func DefaultEmbeddingConfig() EmbeddingConfig {
	return EmbeddingConfig{URI: DefaultEmbeddingURI, SHA256: DefaultEmbeddingSHA256, Pooling: DefaultEmbeddingPooling, QueryInstruction: DefaultQueryInstruction}
}

func CustomEmbeddingConfig(uri string) EmbeddingConfig {
	return EmbeddingConfig{URI: strings.TrimSpace(uri), Pooling: "auto"}
}

func (config EmbeddingConfig) Validate() error {
	if strings.TrimSpace(config.URI) == "" {
		return errors.New("embedding model URI is required")
	}
	parsed, err := url.Parse(config.URI)
	if err != nil || (parsed.Scheme != "hf" && parsed.Scheme != "https") {
		return errors.New("embedding model URI must use hf:// or https://")
	}
	if !strings.HasSuffix(strings.ToLower(parsed.Path), ".gguf") {
		return errors.New("embedding model URI must point to a .gguf file")
	}
	if config.SHA256 != "" {
		if len(config.SHA256) != 64 {
			return errors.New("embedding model SHA-256 must contain 64 hexadecimal characters")
		}
		if _, err := hex.DecodeString(config.SHA256); err != nil {
			return errors.New("embedding model SHA-256 is not hexadecimal")
		}
	}
	switch config.Pooling {
	case "", "auto", "none", "mean", "cls", "last", "rank":
	default:
		return fmt.Errorf("unsupported embedding pooling %q", config.Pooling)
	}
	return nil
}

func (config EmbeddingConfig) Name() string {
	parsed, err := url.Parse(config.URI)
	if err == nil {
		return path.Base(parsed.Path)
	}
	return path.Base(config.URI)
}

func (config EmbeddingConfig) ModelVersion() string {
	return config.SHA256 + ":" + config.Pooling + ":" + fmt.Sprintf("%x", sha256.Sum256([]byte(config.QueryInstruction)))
}

func Load(path string) (Config, error) {
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
	}
	var result Config
	if err := json.Unmarshal(contents, &result); err != nil {
		return Config{}, err
	}
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func Save(path string, value Config) error {
	if err := value.Validate(); err != nil {
		return err
	}
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(contents, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func (value Config) Validate() error {
	if value.Version != CurrentVersion {
		return errors.New("unsupported configuration version")
	}
	if err := value.Embedding.Validate(); err != nil {
		return err
	}
	if len(value.Integrations) == 0 {
		return errors.New("select at least one integration")
	}
	for _, integration := range value.Integrations {
		if integration != "codex" && integration != "claude" {
			return errors.New("unknown integration: " + integration)
		}
	}
	seen := map[string]bool{}
	for _, integration := range value.Integrations {
		if seen[integration] {
			return errors.New("duplicate integration")
		}
		seen[integration] = true
	}
	return nil
}

func (value Config) Enabled(name string) bool {
	return slices.Contains(value.Integrations, name)
}
