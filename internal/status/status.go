package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	appconfig "ragfy/internal/config"
	"ragfy/internal/hook"
	"ragfy/internal/knowledge"
	"ragfy/internal/platform"
)

type Integration struct {
	Name          string `json:"name"`
	Selected      bool   `json:"selected"`
	Executable    string `json:"executable,omitempty"`
	ExecutableOK  bool   `json:"executable_ok"`
	PromptRecall  bool   `json:"prompt_recall"`
	SessionDigest bool   `json:"session_digest"`
	Error         string `json:"error,omitempty"`
}

type Model struct {
	Name    string `json:"name,omitempty"`
	URI     string `json:"uri,omitempty"`
	Pooling string `json:"pooling,omitempty"`
	Path    string `json:"path,omitempty"`
	Size    int64  `json:"size_bytes,omitempty"`
	Cached  bool   `json:"cached"`
}

type Health struct {
	OK        bool                 `json:"ok"`
	Queued    int                  `json:"queued"`
	StartedAt string               `json:"started_at"`
	Version   string               `json:"version"`
	Model     string               `json:"model"`
	Pooling   string               `json:"pooling"`
	Knowledge knowledge.Statistics `json:"knowledge"`
}

type Report struct {
	Version       string                `json:"version"`
	Binary        string                `json:"binary"`
	DataDirectory string                `json:"data_directory"`
	Configured    bool                  `json:"configured"`
	ConfigError   string                `json:"config_error,omitempty"`
	Service       platform.ServiceState `json:"service"`
	Healthy       bool                  `json:"healthy"`
	Ready         bool                  `json:"ready"`
	Health        Health                `json:"health"`
	ServiceError  string                `json:"service_error,omitempty"`
	Startup       string                `json:"startup,omitempty"`
	Model         Model                 `json:"model"`
	Integrations  []Integration         `json:"integrations"`
}

func Inspect(ctx context.Context, version, listenAddress string) Report {
	report := Report{Version: version, Integrations: []Integration{}}
	report.Binary, _ = os.Executable()
	report.DataDirectory, _ = platform.AppDataDir()
	report.Service, _ = platform.InspectService()
	configuration, err := appconfig.LoadRequired(filepath.Join(report.DataDirectory, "config.json"))
	if err != nil {
		if !errors.Is(err, appconfig.ErrNotConfigured) {
			report.ConfigError = err.Error()
		}
		inspectHealth(ctx, &report, listenAddress)
		return report
	}
	report.Configured = true
	report.Model = inspectModel(report.DataDirectory, configuration.Embedding)
	for _, name := range []string{"codex", "claude"} {
		integration := Integration{Name: name, Selected: configuration.Enabled(name)}
		if integration.Selected {
			if executable, executableErr := platform.FindExecutable(name); executableErr == nil {
				integration.Executable, integration.ExecutableOK = executable, true
			} else {
				integration.Error = executableErr.Error()
			}
			if hooks, hookErr := hook.InspectConfiguration(name); hookErr == nil {
				integration.PromptRecall = hooks.PromptRecall
				integration.SessionDigest = hooks.SessionDigest
				if integration.Error == "" && (!hooks.PromptRecall || !hooks.SessionDigest) {
					integration.Error = "Ragfy hooks are missing; run `ragfy setup`"
				}
			} else if integration.Error == "" {
				integration.Error = hookErr.Error()
			}
		}
		report.Integrations = append(report.Integrations, integration)
	}
	inspectHealth(ctx, &report, listenAddress)
	report.Ready = ready(report)
	return report
}

func ready(report Report) bool {
	if !report.Configured || !report.Service.Registered || !report.Service.Running || !report.Healthy || !report.Model.Cached {
		return false
	}
	for _, integration := range report.Integrations {
		if integration.Selected && (!integration.ExecutableOK || !integration.PromptRecall || !integration.SessionDigest) {
			return false
		}
	}
	return true
}

func inspectHealth(ctx context.Context, report *Report, listenAddress string) {
	token, err := os.ReadFile(filepath.Join(report.DataDirectory, "token"))
	if err == nil {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listenAddress+"/health", nil)
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		response, requestErr := (&http.Client{Timeout: 2 * time.Second}).Do(request)
		if requestErr == nil {
			defer response.Body.Close()
			if response.StatusCode/100 == 2 && json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&report.Health) == nil {
				report.Healthy = report.Health.OK
				return
			}
			report.ServiceError = response.Status
		} else {
			report.ServiceError = requestErr.Error()
		}
	}
	logPath := filepath.Join(report.DataDirectory, "logs", "service.log")
	if progress, found := platform.StartupProgress(logPath); found {
		report.Startup = progress
	} else if failure, found := platform.StartupFailure(logPath, time.Time{}); found {
		report.Startup = failure
	}
}

func inspectModel(dataDirectory string, configuration appconfig.EmbeddingConfig) Model {
	result := Model{Name: configuration.Name(), URI: configuration.URI, Pooling: configuration.Pooling}
	modelsDirectory := filepath.Join(dataDirectory, "models")
	_ = filepath.WalkDir(modelsDirectory, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != configuration.Name() {
			return nil
		}
		if info, infoErr := entry.Info(); infoErr == nil {
			result.Path, result.Size, result.Cached = path, info.Size(), true
		}
		return filepath.SkipAll
	})
	return result
}

func Write(writer io.Writer, report Report, asJSON, verbose bool) error {
	if asJSON {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	fmt.Fprintf(writer, "Ragfy %s\n\n", report.Version)
	fmt.Fprintln(writer, "Setup")
	if report.Configured {
		fmt.Fprintln(writer, "  ✓ Configured")
	} else {
		fmt.Fprintln(writer, "  ✗ Not configured")
		if report.ConfigError != "" {
			fmt.Fprintln(writer, "    "+report.ConfigError)
		}
	}
	if verbose || report.Configured {
		fmt.Fprintln(writer, "  Binary    "+report.Binary)
		fmt.Fprintln(writer, "  Data      "+report.DataDirectory)
	}
	if !report.Configured {
		fmt.Fprintln(writer, "\nRun `ragfy setup` to get started.")
		return nil
	}

	fmt.Fprintln(writer, "\nService")
	switch {
	case report.Healthy:
		line := "  ✓ Running and healthy"
		if report.Service.PID != "" {
			line += " (PID " + report.Service.PID + ")"
		}
		fmt.Fprintln(writer, line)
		if started, err := time.Parse(time.RFC3339, report.Health.StartedAt); err == nil {
			fmt.Fprintln(writer, "  Uptime    "+formatDuration(time.Since(started)))
		}
		fmt.Fprintf(writer, "  Queue     %d digestions pending\n", report.Health.Queued)
		if report.Health.Version != "" && report.Health.Version != report.Version {
			fmt.Fprintf(writer, "  ⚠ Service version %s differs from CLI %s; rerun `ragfy setup`\n", report.Health.Version, report.Version)
		} else if verbose && report.Health.Version != "" {
			fmt.Fprintln(writer, "  Version   "+report.Health.Version)
		}
	case report.Service.Running:
		fmt.Fprintln(writer, "  ◌ Running, but the health endpoint is unavailable")
	case report.Service.Registered:
		fmt.Fprintln(writer, "  ✗ Registered, but not running")
	default:
		fmt.Fprintln(writer, "  ✗ Not registered")
	}
	if report.Startup != "" {
		fmt.Fprintln(writer, "  Startup   "+report.Startup)
	} else if verbose && report.ServiceError != "" {
		fmt.Fprintln(writer, "  Error     "+report.ServiceError)
	}

	fmt.Fprintln(writer, "\nEmbedding")
	if report.Model.Cached {
		fmt.Fprintf(writer, "  ✓ %s (%s)\n", report.Model.Name, formatBytes(report.Model.Size))
	} else {
		fmt.Fprintln(writer, "  ✗ Model is not cached: "+report.Model.Name)
	}
	fmt.Fprintln(writer, "  Pooling   "+report.Model.Pooling)
	if verbose {
		fmt.Fprintln(writer, "  URI       "+report.Model.URI)
		if report.Model.Path != "" {
			fmt.Fprintln(writer, "  Cache     "+report.Model.Path)
		}
	}

	fmt.Fprintln(writer, "\nIntegrations")
	for _, integration := range report.Integrations {
		label := map[string]string{"codex": "Codex", "claude": "Claude Code"}[integration.Name]
		if !integration.Selected {
			fmt.Fprintf(writer, "  ○ %-11s not selected\n", label)
			continue
		}
		if integration.ExecutableOK && integration.PromptRecall && integration.SessionDigest {
			fmt.Fprintf(writer, "  ✓ %-11s executable found, hooks configured\n", label)
		} else {
			fmt.Fprintf(writer, "  ✗ %-11s configuration incomplete\n", label)
			if integration.Error != "" {
				fmt.Fprintln(writer, "    "+integration.Error)
			}
		}
	}

	if report.Healthy {
		fmt.Fprintln(writer, "\nKnowledge")
		fmt.Fprintf(writer, "  Active records       %d\n", report.Health.Knowledge.ActiveRecords)
		fmt.Fprintf(writer, "  Superseded records   %d\n", report.Health.Knowledge.SupersededRecords)
		fmt.Fprintf(writer, "  Digested sessions    %d\n", report.Health.Knowledge.DigestedSessions)
		fmt.Fprintf(writer, "  Projects             %d\n", report.Health.Knowledge.Projects)
	}

	if report.Ready {
		fmt.Fprintln(writer, "\nEverything is ready.")
	} else {
		fmt.Fprintln(writer, "\nRun `ragfy setup` to repair the configuration.")
	}
	return nil
}

func formatBytes(size int64) string {
	if size >= 1<<30 {
		return fmt.Sprintf("%.1f GB", float64(size)/float64(1<<30))
	}
	if size >= 1<<20 {
		return fmt.Sprintf("%.0f MB", float64(size)/float64(1<<20))
	}
	return fmt.Sprintf("%d KB", size>>10)
}

func formatDuration(duration time.Duration) string {
	if duration < time.Minute {
		return duration.Round(time.Second).String()
	}
	if duration < time.Hour {
		return duration.Round(time.Minute).String()
	}
	return duration.Round(time.Minute).String()
}
