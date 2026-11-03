package chunking

import (
	"strings"
	"unicode/utf8"
)

type Text struct {
	target  int
	overlap int
}

var _ Chunker = Text{}

func NewText(target, overlap int) Text {
	return Text{target: target, overlap: overlap}
}

func (chunker Text) Split(text string) []string {
	if len(text) <= chunker.target {
		return []string{text}
	}
	var chunks []string
	start := 0
	for start < len(text) {
		end := start + chunker.target
		if end >= len(text) {
			chunks = append(chunks, strings.TrimSpace(text[start:]))
			break
		}
		cut := strings.LastIndex(text[start:end], "\n\n")
		if cut < chunker.target/2 {
			cut = strings.LastIndex(text[start:end], ". ")
		}
		if cut < chunker.target/2 {
			cut = chunker.target
		}
		end = start + cut
		for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		chunks = append(chunks, strings.TrimSpace(text[start:end]))
		start = end - chunker.overlap
		if start < 0 {
			start = 0
		}
		for start > 0 && start < len(text) && !utf8.RuneStart(text[start]) {
			start++
		}
		for start < len(text) && (text[start] == ' ' || text[start] == '\n') {
			start++
		}
	}
	return chunks
}
