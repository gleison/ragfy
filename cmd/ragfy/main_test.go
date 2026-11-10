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
	for _, expected := range []string{"Find stored memories", "Extract durable knowledge", "ragfy help <command>"} {
		if !strings.Contains(global.String(), expected) {
			t.Fatalf("global help missing %q: %s", expected, global.String())
		}
	}
	var ingest bytes.Buffer
	if !writeCommandHelp(&ingest, "ingest") || !strings.Contains(ingest.String(), "--text") || !strings.Contains(ingest.String(), "same pipeline as session digest") || !strings.Contains(ingest.String(), "only configured provider is selected automatically") {
		t.Fatalf("ingest help = %q", ingest.String())
	}
}

func TestHelpDescribesSessionAndRecallAudit(t *testing.T) {
	var global bytes.Buffer
	writeGlobalHelp(&global)
	if !strings.Contains(global.String(), "List, inspect, or forget") || !strings.Contains(global.String(), "memories injected") {
		t.Fatalf("global help = %q", global.String())
	}
	var sessions, recalls bytes.Buffer
	if !writeCommandHelp(&sessions, "sessions") || !strings.Contains(sessions.String(), "sessions forget") || !strings.Contains(sessions.String(), "reactivates") {
		t.Fatalf("sessions help = %q", sessions.String())
	}
	if !writeCommandHelp(&recalls, "recalls") || !strings.Contains(recalls.String(), "ranking scores") {
		t.Fatalf("recalls help = %q", recalls.String())
	}
}

func TestJSONCommandArgumentsAcceptsFlagBeforeOrAfterIdentifier(t *testing.T) {
	for _, arguments := range [][]string{{"--json", "session"}, {"session", "--json"}} {
		remaining, asJSON, err := jsonCommandArguments(arguments)
		if err != nil || !asJSON || len(remaining) != 1 || remaining[0] != "session" {
			t.Fatalf("arguments=%#v remaining=%#v json=%v err=%v", arguments, remaining, asJSON, err)
		}
	}
}
