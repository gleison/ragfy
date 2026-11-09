package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCommandArgumentsHandlesDefaultStatusInvocation(t *testing.T) {
	if arguments := commandArguments([]string{"ragfy"}); len(arguments) != 0 {
		t.Fatalf("default arguments = %#v", arguments)
	}
	arguments := commandArguments([]string{"ragfy", "status", "--json"})
	if len(arguments) != 1 || arguments[0] != "--json" {
		t.Fatalf("status arguments = %#v", arguments)
	}
}

func TestHelpDescribesSearchAndTextIngest(t *testing.T) {
	var global bytes.Buffer
	writeGlobalHelp(&global)
	for _, expected := range []string{"Find stored memories", "save text directly", "ragfy help <command>"} {
		if !strings.Contains(global.String(), expected) {
			t.Fatalf("global help missing %q: %s", expected, global.String())
		}
	}
	var ingest bytes.Buffer
	if !writeCommandHelp(&ingest, "ingest") || !strings.Contains(ingest.String(), "--text") || !strings.Contains(ingest.String(), "generative session extraction") {
		t.Fatalf("ingest help = %q", ingest.String())
	}
}
