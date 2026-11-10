package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readHookConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	return root
}

func hookCommands(t *testing.T, root map[string]any, event string) []map[string]any {
	t.Helper()
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("missing hooks object: %#v", root)
	}
	entries, _ := hooks[event].([]any)
	var commands []map[string]any
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		children, _ := entry["hooks"].([]any)
		for _, child := range children {
			if command, ok := child.(map[string]any); ok {
				commands = append(commands, command)
			}
		}
	}
	return commands
}

func TestMergeHookFilePreservesUnrelatedHooksAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := `{
  "description": "keep me",
  "hooks": {
    "UserPromptSubmit": [
      {"hooks":[{"type":"command","command":"python3 /old/recall.py"}]},
      {"hooks":[{"type":"command","command":"/usr/local/bin/unrelated"}]}
    ],
    "PostToolUse": [{"matcher":"Bash","hooks":[{"type":"command","command":"audit"}]}]
  }
}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	exe := "/Applications/Ragfy Tools/ragfy"
	if err := MergeConfig(path, exe, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := MergeConfig(path, exe, "codex"); err != nil {
		t.Fatal(err)
	}

	root := readHookConfig(t, path)
	if root["description"] != "keep me" {
		t.Fatalf("unrelated root data was lost: %#v", root)
	}
	commands := hookCommands(t, root, "UserPromptSubmit")
	recallCommand := `"/Applications/Ragfy Tools/ragfy" hook recall codex`
	counts := map[string]int{}
	for _, command := range commands {
		value, _ := command["command"].(string)
		counts[value]++
		if value == recallCommand && command["timeout"] != float64(4) {
			t.Fatalf("recall timeout = %#v", command["timeout"])
		}
	}
	if counts["python3 /old/recall.py"] != 0 || counts["/usr/local/bin/unrelated"] != 1 || counts[recallCommand] != 1 {
		t.Fatalf("unexpected prompt hook commands: %#v", counts)
	}

	digestCommands := hookCommands(t, root, "SessionEnd")
	if len(digestCommands) != 1 || digestCommands[0]["command"] != `"/Applications/Ragfy Tools/ragfy" hook digest codex` || digestCommands[0]["timeout"] != float64(2) {
		t.Fatalf("unexpected digest hook: %#v", digestCommands)
	}
	if len(hookCommands(t, root, "PostToolUse")) != 1 {
		t.Fatalf("unrelated event was modified: %#v", root)
	}
	bootstrapCommands := hookCommands(t, root, "SessionStart")
	if len(bootstrapCommands) != 1 || bootstrapCommands[0]["command"] != `"/Applications/Ragfy Tools/ragfy" hook bootstrap codex` || bootstrapCommands[0]["timeout"] != float64(4) {
		t.Fatalf("unexpected bootstrap hook: %#v", bootstrapCommands)
	}

	backup, err := os.ReadFile(path + ".ragfy.bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != original {
		t.Fatal("backup does not contain the original hook file")
	}
}

func TestMergeHookFileCreatesConfigAndRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	created := filepath.Join(dir, "new", "hooks.json")
	if err := MergeConfig(created, "/usr/local/bin/ragfy", "claude"); err != nil {
		t.Fatal(err)
	}
	root := readHookConfig(t, created)
	if len(hookCommands(t, root, "SessionStart")) != 1 || len(hookCommands(t, root, "UserPromptSubmit")) != 1 || len(hookCommands(t, root, "SessionEnd")) != 1 {
		t.Fatalf("new config is incomplete: %#v", root)
	}

	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"hooks":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := MergeConfig(invalid, "/usr/local/bin/ragfy", "claude"); err == nil {
		t.Fatal("invalid JSON hook config was accepted")
	}
}

func TestRemoveConfigRemovesOnlyRagfyHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	contents := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"\"/usr/local/bin/ragfy\" hook bootstrap codex"}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/usr/local/bin/unrelated"},{"type":"command","command":"\"/usr/local/bin/ragfy\" hook recall codex"}]}],"SessionEnd":[{"hooks":[{"type":"command","command":"\"/usr/local/bin/ragfy\" hook digest codex"}]}]}}`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveConfig(path, "codex"); err != nil {
		t.Fatal(err)
	}
	root := readHookConfig(t, path)
	commands := hookCommands(t, root, "UserPromptSubmit")
	if len(commands) != 1 || commands[0]["command"] != "/usr/local/bin/unrelated" {
		t.Fatalf("unexpected remaining hooks: %#v", root)
	}
	if len(hookCommands(t, root, "SessionEnd")) != 0 {
		t.Fatalf("digest hook was not removed: %#v", root)
	}
	if len(hookCommands(t, root, "SessionStart")) != 0 {
		t.Fatalf("bootstrap hook was not removed: %#v", root)
	}
}

func TestInspectConfigurationReportsBothHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".codex", "hooks.json")
	executable := filepath.Join(home, "bin", "ragfy")
	if err := os.MkdirAll(filepath.Dir(executable), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := MergeConfig(path, executable, "codex"); err != nil {
		t.Fatal(err)
	}
	state, err := InspectConfiguration("codex")
	if err != nil {
		t.Fatal(err)
	}
	if !state.SessionBootstrap || !state.PromptRecall || !state.SessionDigest || !state.ExecutableOK || state.Executable != executable || state.Path != path {
		t.Fatalf("configuration state = %#v", state)
	}
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	state, err = InspectConfiguration("codex")
	if err != nil {
		t.Fatal(err)
	}
	if state.ExecutableOK {
		t.Fatalf("missing hook executable reported healthy: %#v", state)
	}
}

func TestConfigureRejectsMissingExecutable(t *testing.T) {
	if err := Configure(filepath.Join(t.TempDir(), "missing-ragfy"), []string{"codex"}); err == nil {
		t.Fatal("missing hook executable was accepted")
	}
}
