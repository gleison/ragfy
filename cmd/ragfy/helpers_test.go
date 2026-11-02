package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestAppDataDirHonorsRagfyHomeAndLoadTokenIsStable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RAGFY_HOME", dir)
	got, err := appDataDir()
	if err != nil || got != dir {
		t.Fatalf("appDataDir = %q, %v", got, err)
	}
	tokenPath := filepath.Join(dir, "token")
	first, err := loadToken(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadToken(tokenPath)
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
	message, ok := startupProgress(path)
	if !ok || !strings.Contains(message, "loading model Qwen") || !strings.Contains(message, "ago") {
		t.Fatalf("progress = %q, %v", message, ok)
	}
	if err := os.WriteFile(path, []byte(stamp+" startup: loading model Qwen\n"+stamp+" startup: ready; ragfy active\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if message, ok := startupProgress(path); ok || message != "" {
		t.Fatalf("ready service reported progress = %q, %v", message, ok)
	}
}

func TestQueryTermsSupportsMultilingualTextAndDeduplicates(t *testing.T) {
	terms := queryTerms("Como o serviço persistente evita recarregamento? Serviço multilíngue.")
	joined := strings.Join(terms, "|")
	for _, want := range []string{"como", "serviço", "persistente", "evita", "recarregamento", "multilíngue"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in terms %#v", want, terms)
		}
	}
	count := 0
	for _, term := range terms {
		if term == "serviço" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("serviço appears %d times in %#v", count, terms)
	}
}

func TestSplitChunksPreservesUTF8AndOverlap(t *testing.T) {
	text := strings.Repeat("Conhecimento multilíngue persiste entre sessões. ", 20)
	chunks := splitChunks(text, 180, 30)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want multiple", len(chunks))
	}
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk) || strings.TrimSpace(chunk) == "" {
			t.Fatalf("invalid chunk %q", chunk)
		}
	}
}

func TestVectorAndRankingHelpers(t *testing.T) {
	if got := cosine([]float32{1, 0}, []float32{1, 0}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("cosine identical = %f", got)
	}
	if got := cosine([]float32{1}, []float32{1, 0}); got != 0 {
		t.Fatalf("cosine mismatched dimensions = %f", got)
	}
	if got := lexicalScore([]string{"daemon", "sqlite"}, "The daemon is persistent"); got != 0.5 {
		t.Fatalf("lexical score = %f", got)
	}
	hits := []Hit{{Score: 0.2}, {Score: 0.9}, {Score: 0.5}}
	sortHits(hits)
	if hits[0].Score != 0.9 || hits[2].Score != 0.2 {
		t.Fatalf("hits not sorted: %#v", hits)
	}

	buffer := make([]byte, 4)
	putFloat(buffer, 3.25)
	if got := getFloat(buffer); got != 3.25 {
		t.Fatalf("float round trip = %f", got)
	}
}

func TestVerifyModelDetectsChecksumMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.gguf")
	content := []byte("small deterministic model fixture")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyModel(path, sha256Hex(content)); err != nil {
		t.Fatal(err)
	}
	if err := verifyModel(path, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("checksum mismatch error = %v", err)
	}
}
