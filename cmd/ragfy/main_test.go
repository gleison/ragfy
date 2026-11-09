package main

import "testing"

func TestCommandArgumentsHandlesDefaultStatusInvocation(t *testing.T) {
	if arguments := commandArguments([]string{"ragfy"}); len(arguments) != 0 {
		t.Fatalf("default arguments = %#v", arguments)
	}
	arguments := commandArguments([]string{"ragfy", "status", "--json"})
	if len(arguments) != 1 || arguments[0] != "--json" {
		t.Fatalf("status arguments = %#v", arguments)
	}
}
