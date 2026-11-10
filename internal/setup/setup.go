package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	selected, err := askConfiguration(input, output, current)
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
	if err := platform.WaitHealthy(waitContext, settings.ListenAddress, serviceStartedAt, output); err != nil {
		return err
	}
	fmt.Fprintln(output, "Restart active Codex or Claude Code sessions so they reload the updated hook commands.")
	return nil
}

func Uninstall(executable string, purge bool) error {
	var uninstallErrors []error
	uninstallErrors = append(uninstallErrors,
		wrapUninstallError("remove service", platform.UninstallService()),
		wrapUninstallError("remove hooks", hook.RemoveAll()),
	)
	if purge {
		dataDirectory, err := platform.AppDataDir()
		if err != nil {
			uninstallErrors = append(uninstallErrors, wrapUninstallError("purge local data", err))
		} else {
			uninstallErrors = append(uninstallErrors, wrapUninstallError("purge local data", removeDataDirectory(dataDirectory)))
		}
	}
	uninstallErrors = append(uninstallErrors, wrapUninstallError("remove executable", removeExecutable(executable)))
	return errors.Join(uninstallErrors...)
}

func wrapUninstallError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

func removeDataDirectory(directory string) error {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	clean := filepath.Clean(absolute)
	home, _ := os.UserHomeDir()
	if clean == filepath.Clean(string(filepath.Separator)) || home != "" && clean == filepath.Clean(home) {
		return fmt.Errorf("refusing to purge unsafe data directory %q", clean)
	}
	return os.RemoveAll(clean)
}

func removeExecutable(executable string) error {
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return err
	}
	name := strings.ToLower(filepath.Base(absolute))
	if name != "ragfy" && name != "ragfy.exe" {
		return fmt.Errorf("refusing to remove unexpected executable %q", absolute)
	}
	if isHomebrewExecutable(absolute) {
		return errors.New("this installation is managed by Homebrew; run `brew uninstall ragfy`")
	}
	if err := os.Remove(absolute); err == nil || os.IsNotExist(err) {
		return nil
	} else if !os.IsPermission(err) || runtime.GOOS == "windows" {
		return err
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("removing %s requires elevated permissions", absolute)
	}
	if err := exec.Command("sudo", "rm", "-f", "--", absolute).Run(); err != nil {
		return fmt.Errorf("remove %s with sudo: %w", absolute, err)
	}
	return nil
}

func isHomebrewExecutable(path string) bool {
	normalized := filepath.ToSlash(strings.ToLower(path))
	return strings.Contains(normalized, "/cellar/") || strings.Contains(normalized, "/homebrew/")
}
