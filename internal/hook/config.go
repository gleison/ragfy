package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func Setup(executable string) error {
	return Configure(executable, []string{"claude", "codex"})
}

func Configure(executable string, enabled []string) error {
	if err := validateExecutable(executable); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, target := range []struct{ path, source string }{
		{filepath.Join(home, ".claude", "settings.json"), "claude"},
		{filepath.Join(home, ".codex", "hooks.json"), "codex"},
	} {
		selected := false
		for _, source := range enabled {
			selected = selected || source == target.source
		}
		var configureErr error
		if selected {
			configureErr = MergeConfig(target.path, executable, target.source)
		} else {
			configureErr = RemoveConfig(target.path, target.source, executable)
		}
		if configureErr != nil {
			return configureErr
		}
		if selected {
			fmt.Println("Hooks updated:", target.path)
		} else {
			fmt.Println("Hooks removed:", target.path)
		}
	}
	return nil
}

func RemoveAll() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, target := range []struct{ path, source string }{
		{filepath.Join(home, ".claude", "settings.json"), "claude"},
		{filepath.Join(home, ".codex", "hooks.json"), "codex"},
	} {
		if err := RemoveConfig(target.path, target.source); err != nil {
			return err
		}
	}
	return nil
}

type ConfigurationState struct {
	Path             string `json:"path"`
	SessionBootstrap bool   `json:"session_bootstrap"`
	ContextReset     bool   `json:"context_reset"`
	PromptRecall     bool   `json:"prompt_recall"`
	SessionDigest    bool   `json:"session_digest"`
	Executable       string `json:"executable,omitempty"`
	ExecutableOK     bool   `json:"executable_ok"`
}

func InspectConfiguration(source string) (ConfigurationState, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ConfigurationState{}, err
	}
	path := ""
	switch source {
	case "claude":
		path = filepath.Join(home, ".claude", "settings.json")
	case "codex":
		path = filepath.Join(home, ".codex", "hooks.json")
	default:
		return ConfigurationState{}, fmt.Errorf("unknown hook source %q", source)
	}
	state := ConfigurationState{Path: path}
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	var root map[string]any
	if err := json.Unmarshal(contents, &root); err != nil {
		return state, fmt.Errorf("invalid hook config: %s", path)
	}
	hooks, _ := root["hooks"].(map[string]any)
	bootstrapExecutable, bootstrapFound := eventRagfyExecutable(hooks, "SessionStart", "bootstrap", source)
	resetExecutable, resetFound := eventRagfyExecutable(hooks, "PreCompact", "reset", source)
	promptExecutable, promptFound := eventRagfyExecutable(hooks, "UserPromptSubmit", "recall", source)
	digestExecutable, digestFound := eventRagfyExecutable(hooks, "SessionEnd", "digest", source)
	state.SessionBootstrap = bootstrapFound
	state.ContextReset = resetFound
	state.PromptRecall = promptFound
	state.SessionDigest = digestFound
	if bootstrapFound && resetFound && promptFound && digestFound && bootstrapExecutable == resetExecutable && resetExecutable == promptExecutable && promptExecutable == digestExecutable {
		state.Executable = promptExecutable
		state.ExecutableOK = validateExecutable(promptExecutable) == nil
	}
	return state, nil
}

func eventRagfyExecutable(hooks map[string]any, event, action, source string) (string, bool) {
	entries, _ := hooks[event].([]any)
	for _, rawEntry := range entries {
		entry, _ := rawEntry.(map[string]any)
		children, _ := entry["hooks"].([]any)
		for _, rawChild := range children {
			child, _ := rawChild.(map[string]any)
			command, _ := child["command"].(string)
			suffix := " hook " + action + " " + source
			if strings.HasSuffix(command, suffix) && isRagfyHookCommand(command, source) {
				executable := strings.TrimSpace(strings.TrimSuffix(command, suffix))
				return strings.Trim(executable, `"`), true
			}
		}
	}
	return "", false
}

func validateExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("hook executable %q is unavailable: %w", path, err)
	}
	if info.IsDir() || runtime.GOOS != "windows" && info.Mode()&0111 == 0 {
		return fmt.Errorf("hook executable %q is not executable", path)
	}
	return nil
}

func RemoveConfig(path, source string, executables ...string) error {
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	backupPath := path + ".ragfy.bak"
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		_ = os.WriteFile(backupPath, contents, 0600)
	}
	var root map[string]any
	if err := json.Unmarshal(contents, &root); err != nil {
		return fmt.Errorf("invalid hook config: %s", path)
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}
	for _, executable := range executables {
		removeExecutableHooks(hooks, executable)
	}
	removeRagfyHooks(hooks, source)
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0600)
}

func MergeConfig(path, executable, source string) error {
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
	removeRagfyHooks(hooks, source)
	removeExecutableHooks(hooks, executable)
	for event, action := range map[string]string{"SessionStart": "bootstrap", "PreCompact": "reset", "UserPromptSubmit": "recall", "SessionEnd": "digest"} {
		command := strconv.Quote(executable) + " hook " + action + " " + source
		timeout := 4
		if event == "SessionEnd" || event == "PreCompact" {
			timeout = 2
		}
		entries, _ := hooks[event].([]any)
		hooks[event] = append(entries, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": timeout}}})
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

func removeExecutableHooks(hooks map[string]any, executable string) {
	prefix := strconv.Quote(executable) + " hook "
	for _, event := range []string{"SessionStart", "PreCompact", "UserPromptSubmit", "SessionEnd"} {
		entries, _ := hooks[event].([]any)
		keptEntries := make([]any, 0, len(entries))
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				keptEntries = append(keptEntries, rawEntry)
				continue
			}
			children, _ := entry["hooks"].([]any)
			keptChildren := make([]any, 0, len(children))
			for _, rawChild := range children {
				child, _ := rawChild.(map[string]any)
				command, _ := child["command"].(string)
				if !strings.HasPrefix(command, prefix) {
					keptChildren = append(keptChildren, rawChild)
				}
			}
			if len(keptChildren) > 0 {
				entry["hooks"] = keptChildren
				keptEntries = append(keptEntries, entry)
			}
		}
		hooks[event] = keptEntries
	}
}

func removeRagfyHooks(hooks map[string]any, source string) {
	for _, event := range []string{"SessionStart", "PreCompact", "UserPromptSubmit", "SessionEnd"} {
		entries, _ := hooks[event].([]any)
		keptEntries := make([]any, 0, len(entries))
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				keptEntries = append(keptEntries, rawEntry)
				continue
			}
			children, _ := entry["hooks"].([]any)
			keptChildren := make([]any, 0, len(children))
			for _, rawChild := range children {
				child, _ := rawChild.(map[string]any)
				command, _ := child["command"].(string)
				if !isRagfyHookCommand(command, source) {
					keptChildren = append(keptChildren, rawChild)
				}
			}
			if len(keptChildren) > 0 {
				entry["hooks"] = keptChildren
				keptEntries = append(keptEntries, entry)
			}
		}
		hooks[event] = keptEntries
	}
}

func isRagfyHookCommand(command, source string) bool {
	for _, action := range []string{"bootstrap", "reset", "recall", "digest"} {
		suffix := " hook " + action + " " + source
		if !strings.HasSuffix(command, suffix) {
			continue
		}
		executable := strings.TrimSpace(strings.TrimSuffix(command, suffix))
		executable = strings.Trim(executable, `"`)
		return strings.EqualFold(filepath.Base(executable), "ragfy")
	}
	return false
}
