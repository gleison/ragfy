package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenChunksNeverExceedBatchSize(t *testing.T) {
	chunks := tokenChunks(2897, 2048)
	expected := [][2]int{{0, 2048}, {2048, 2897}}
	if len(chunks) != len(expected) {
		t.Fatalf("got %d chunks, want %d: %#v", len(chunks), len(expected), chunks)
	}
	for index := range expected {
		if chunks[index] != expected[index] {
			t.Fatalf("chunk %d = %#v, want %#v", index, chunks[index], expected[index])
		}
	}
}

func TestVerifyFileDetectsChecksumMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.gguf")
	contents := []byte("small deterministic model fixture")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyFile(path, "2b5bd846a7cc2c5a04cbbe4977aabb35992530f430dc895ff6965864ffbae75a"); err != nil {
		t.Fatal(err)
	}
	if err := verifyFile(path, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("checksum mismatch error = %v", err)
	}
}
