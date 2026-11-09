package updater

import "testing"

func TestInstallationPrefixSupportsDirectInstalls(t *testing.T) {
	prefix, err := installationPrefix("/usr/local/bin/ragfy")
	if err != nil || prefix != "/usr/local" {
		t.Fatalf("installationPrefix = %q, %v", prefix, err)
	}
	if _, err := installationPrefix("/tmp/ragfy"); err == nil {
		t.Fatal("development executable was accepted for self-update")
	}
}
