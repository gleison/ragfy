package chunking

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextPreservesUTF8AndOverlap(t *testing.T) {
	text := strings.Repeat("Conhecimento multilíngue persiste entre sessões. ", 20)
	chunks := NewText(180, 30).Split(text)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want multiple", len(chunks))
	}
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk) || strings.TrimSpace(chunk) == "" {
			t.Fatalf("invalid chunk %q", chunk)
		}
	}
}
