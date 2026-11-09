package status

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"ragfy/internal/knowledge"
	"ragfy/internal/platform"
)

func TestWriteReportsActionBeforeSetup(t *testing.T) {
	var output bytes.Buffer
	if err := Write(&output, Report{Version: "0.2.0", Binary: "/usr/local/bin/ragfy", DataDirectory: "/data"}, false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Not configured") || !strings.Contains(output.String(), "ragfy setup") {
		t.Fatalf("status output = %q", output.String())
	}
}

func TestWriteReportsHealthyRuntimeAndKnowledge(t *testing.T) {
	report := Report{
		Version:    "0.2.0",
		Configured: true,
		Healthy:    true,
		Ready:      true,
		Service:    platform.ServiceState{Registered: true, Running: true},
		Health: Health{
			OK:        true,
			StartedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			Knowledge: knowledge.Statistics{ActiveRecords: 4, SupersededRecords: 2, DigestedSessions: 3, Projects: 1},
		},
		Model:        Model{Name: "embedding.gguf", Cached: true, Size: 10 << 20, Pooling: "last"},
		Integrations: []Integration{{Name: "codex", Selected: true, ExecutableOK: true, PromptRecall: true, SessionDigest: true}},
	}
	var output bytes.Buffer
	if err := Write(&output, report, false, false); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Running and healthy", "embedding.gguf", "Codex", "Active records       4", "Everything is ready"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("status output missing %q: %q", expected, output.String())
		}
	}
}
