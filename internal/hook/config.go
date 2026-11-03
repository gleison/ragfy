package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func Setup(executable string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".codex", "hooks.json")} {
		if err := MergeConfig(path, executable); err != nil {
			return err
		}
		fmt.Println("Hooks atualizados:", path)
	}
	return nil
}

func MergeConfig(path, executable string) error {
	root, err := readConfig(path)
	if err != nil {
		return err
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	removeLegacyPythonRecallHooks(hooks)
	for event, action := range map[string]string{"UserPromptSubmit": "recall", "SessionEnd": "digest"} {
		command := strconv.Quote(executable) + " hook " + action
		timeout := 4
		if event == "SessionEnd" {
			timeout = 2
		}
		entries, _ := hooks[event].([]any)
		if !containsCommand(entries, command) {
			hooks[event] = append(entries, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": timeout}}})
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0600)
}

func readConfig(path string) (map[string]any, error) {
	var root map[string]any
	contents, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(contents, &root) != nil {
			return nil, fmt.Errorf("invalid hook config: %s", path)
		}
		backupPath := path + ".ragfy.bak"
		if _, err := os.Stat(backupPath); os.IsNotExist(err) {
			_ = os.WriteFile(backupPath, contents, 0600)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, nil
}

func containsCommand(entries []any, command string) bool {
	for _, rawEntry := range entries {
		entry, _ := rawEntry.(map[string]any)
		hooks, _ := entry["hooks"].([]any)
		for _, rawHook := range hooks {
			hook, _ := rawHook.(map[string]any)
			if hook["command"] == command {
				return true
			}
		}
	}
	return false
}

func removeLegacyPythonRecallHooks(hooks map[string]any) {
	entries, ok := hooks["UserPromptSubmit"].([]any)
	if !ok {
		return
	}
	keptEntries := make([]any, 0, len(entries))
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			keptEntries = append(keptEntries, rawEntry)
			continue
		}
		children, _ := entry["hooks"].([]any)
		keptHooks := make([]any, 0, len(children))
		for _, child := range children {
			hook, _ := child.(map[string]any)
			command, _ := hook["command"].(string)
			if !strings.Contains(command, "recall.py") {
				keptHooks = append(keptHooks, child)
			}
		}
		if len(keptHooks) > 0 {
			entry["hooks"] = keptHooks
			keptEntries = append(keptEntries, entry)
		}
	}
	hooks["UserPromptSubmit"] = keptEntries
}
