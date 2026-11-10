package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	agentTimeout     = 3 * time.Minute
	maximumAgentLog  = 8 << 10
	maximumAgentText = 256 << 10
)

type AgentGenerator struct {
	name       string
	executable string
	directory  string
	arguments  []string
	parse      func([]byte) (string, error)
}

var _ Generator = (*AgentGenerator)(nil)

func NewCodexGenerator(executable, directory, schemaPath string) *AgentGenerator {
	return &AgentGenerator{
		name:       "codex",
		executable: executable,
		directory:  directory,
		arguments: []string{
			"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--disable", "hooks",
			"--sandbox", "read-only", "--skip-git-repo-check", "--output-schema", schemaPath,
			"--color", "never", "-C", directory, "-",
		},
		parse: parseCodexOutput,
	}
}

func NewClaudeGenerator(executable, directory, schema string) *AgentGenerator {
	return &AgentGenerator{
		name:       "claude",
		executable: executable,
		directory:  directory,
		arguments: []string{
			"-p", "--safe-mode", "--restricted", "--tools", "", "--permission-mode", "dontAsk",
			"--permission-prompts", "none", "--no-session-persistence", "--output-format", "json",
			"--json-schema", schema,
		},
		parse: parseClaudeOutput,
	}
}

func (generator *AgentGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, generator.executable, generator.arguments...)
	command.Dir = generator.directory
	command.Env = append(os.Environ(), "RAGFY_AGENT_EXEC=1")
	command.Stdin = strings.NewReader(prompt)
	stdout := newLimitedBuffer(maximumAgentText)
	stderr := newLimitedBuffer(maximumAgentLog)
	command.Stdout, command.Stderr = stdout, stderr
	started := time.Now()
	err := command.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("%s extraction timed out after %s", generator.name, time.Since(started).Round(time.Second))
	}
	if err != nil {
		return "", fmt.Errorf("%s extraction failed: %w: %s", generator.name, err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Exceeded() {
		return "", fmt.Errorf("%s extraction exceeded %d bytes", generator.name, maximumAgentText)
	}
	result, err := generator.parse(stdout.Bytes())
	if err != nil {
		return "", fmt.Errorf("parse %s extraction: %w", generator.name, err)
	}
	return result, nil
}

func parseCodexOutput(output []byte) (string, error) {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return "", errors.New("empty output")
	}
	return text, nil
}

func parseClaudeOutput(output []byte) (string, error) {
	var response struct {
		IsError          bool            `json:"is_error"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return "", fmt.Errorf("invalid JSON envelope: %w", err)
	}
	if response.IsError {
		return "", fmt.Errorf("agent returned an error: %s", strings.TrimSpace(response.Result))
	}
	if len(response.StructuredOutput) > 0 && string(response.StructuredOutput) != "null" {
		return string(response.StructuredOutput), nil
	}
	if strings.TrimSpace(response.Result) == "" {
		return "", errors.New("missing structured_output")
	}
	return response.Result, nil
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{limit: limit}
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return written, nil
	}
	if len(data) > remaining {
		_, _ = buffer.buffer.Write(data[:remaining])
		buffer.exceeded = true
		return written, nil
	}
	_, _ = buffer.buffer.Write(data)
	return written, nil
}

func (buffer *limitedBuffer) Bytes() []byte  { return buffer.buffer.Bytes() }
func (buffer *limitedBuffer) String() string { return buffer.buffer.String() }
func (buffer *limitedBuffer) Exceeded() bool { return buffer.exceeded }
