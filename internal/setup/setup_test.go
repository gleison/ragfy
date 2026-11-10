package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveDataDirectoryRemovesContents(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "ragfy-data")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeDataDirectory(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("data directory still exists: %v", err)
	}
}

func TestRemoveDataDirectoryRejectsRootAndHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{string(filepath.Separator), home} {
		if err := removeDataDirectory(directory); err == nil {
			t.Fatalf("unsafe directory %q was accepted", directory)
		}
	}
}

func TestRemoveExecutableRemovesDirectInstallation(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "ragfy")
	if err := os.WriteFile(executable, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := removeExecutable(executable); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(executable); !os.IsNotExist(err) {
		t.Fatalf("executable still exists: %v", err)
	}
	if err := removeExecutable(executable); err != nil {
		t.Fatalf("second removal should be idempotent: %v", err)
	}
}

func TestRemoveExecutableRejectsUnexpectedAndHomebrewPaths(t *testing.T) {
	if err := removeExecutable(filepath.Join(t.TempDir(), "not-ragfy")); err == nil {
		t.Fatal("unexpected executable name was accepted")
	}
	if err := removeExecutable("/opt/homebrew/Cellar/ragfy/1.0/bin/ragfy"); err == nil {
		t.Fatal("Homebrew executable was accepted")
	}
}
