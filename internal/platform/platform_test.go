package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppDataDirHonorsRagfyHomeAndLoadTokenIsStable(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("RAGFY_HOME", directory)
	actualDirectory, err := AppDataDir()
	if err != nil || actualDirectory != directory {
		t.Fatalf("AppDataDir = %q, %v", actualDirectory, err)
	}
	tokenPath := filepath.Join(directory, "token")
	first, err := LoadToken(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadToken(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second || len(first) != 64 {
		t.Fatalf("unstable token: first=%q second=%q", first, second)
	}
}

func TestStartupProgressReportsActiveStartupButNotReadyService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	stamp := time.Now().Format("2006/01/02 15:04:05")
	if err := os.WriteFile(path, []byte(stamp+" startup: loading model Qwen\n"), 0600); err != nil {
		t.Fatal(err)
	}
	message, found := StartupProgress(path)
	if !found || !strings.Contains(message, "loading model Qwen") || !strings.Contains(message, "ago") {
		t.Fatalf("progress = %q, %v", message, found)
	}
	if err := os.WriteFile(path, []byte(stamp+" startup: loading model Qwen\n"+stamp+" startup: ready; ragfy active\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if message, found := StartupProgress(path); found || message != "" {
		t.Fatalf("ready service reported progress = %q, %v", message, found)
	}
}
