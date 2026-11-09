package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	appconfig "ragfy/internal/config"
	"ragfy/internal/hook"
	"ragfy/internal/model"
	"ragfy/internal/platform"
	"ragfy/internal/settings"
)

func Run(ctx context.Context, executable string, input io.Reader, output io.Writer) error {
	dataDirectory, err := platform.AppDataDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(dataDirectory, "config.json")
	current, err := appconfig.Load(configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	selected, err := NewWizard(input, output).Ask(current)
	if err != nil {
		return err
	}
	for _, integration := range selected.Integrations {
		if _, err := platform.FindExecutable(integration); err != nil {
			return fmt.Errorf("selected integration is unavailable: %w", err)
		}
	}

	fmt.Fprintln(output, "\nValidating embedding model (it will be downloaded if absent)…")
	engine, err := model.NewEmbeddingLlamaWithConfig(ctx, filepath.Join(dataDirectory, "models"), selected.Embedding)
	if err != nil {
		return fmt.Errorf("validate embedding model: %w", err)
	}
	if err := engine.ValidateEmbedding(ctx); err != nil {
		engine.Close()
		return fmt.Errorf("model cannot be used for embeddings: %w", err)
	}
	selected.Embedding.SHA256 = engine.SHA256()
	if err := engine.Close(); err != nil {
		return err
	}
	fmt.Fprintln(output, "✓ Embedding model validated")

	if err := appconfig.Save(configPath, selected); err != nil {
		return fmt.Errorf("save configuration: %w", err)
	}
	fmt.Fprintln(output, "✓ Configuration saved:", configPath)
	if err := hook.Configure(executable, selected.Integrations); err != nil {
		return fmt.Errorf("configure hooks: %w", err)
	}
	serviceStartedAt := time.Now()
	if err := platform.InstallService(executable); err != nil {
		return fmt.Errorf("install service: %w", err)
	}
	fmt.Fprintln(output, "Waiting for the Ragfy service…")
	waitContext, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	return platform.WaitHealthy(waitContext, settings.ListenAddress, serviceStartedAt, output)
}

func Uninstall() error {
	return errors.Join(platform.UninstallService(), hook.RemoveAll())
}
