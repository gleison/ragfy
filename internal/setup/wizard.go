package setup

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	appconfig "ragfy/internal/config"
)

type Wizard struct {
	input  *bufio.Reader
	output io.Writer
}

func NewWizard(input io.Reader, output io.Writer) *Wizard {
	return &Wizard{input: bufio.NewReader(input), output: output}
}

func (wizard *Wizard) Ask(current appconfig.Config) (appconfig.Config, error) {
	fmt.Fprintln(wizard.output, "Ragfy setup")
	fmt.Fprintln(wizard.output, "")
	fmt.Fprintf(wizard.output, "Embedding model URI [%s]: ", current.Embedding.URI)
	uri, err := wizard.readLine()
	if err != nil {
		return appconfig.Config{}, err
	}
	if uri == "" {
		uri = current.Embedding.URI
	}
	embedding := current.Embedding
	if uri != current.Embedding.URI {
		if uri == appconfig.DefaultEmbeddingURI {
			embedding = appconfig.DefaultEmbeddingConfig()
		} else {
			embedding = appconfig.CustomEmbeddingConfig(uri)
		}
	}
	if err := embedding.Validate(); err != nil {
		return appconfig.Config{}, err
	}

	fmt.Fprintln(wizard.output, "\nIntegrations (enter a comma-separated selection):")
	fmt.Fprintf(wizard.output, "  [%s] 1. Codex\n", checkbox(current.Enabled("codex")))
	fmt.Fprintf(wizard.output, "  [%s] 2. Claude Code\n", checkbox(current.Enabled("claude")))
	fmt.Fprintf(wizard.output, "Selection [%s]: ", selectionLabel(current.Integrations))
	selection, err := wizard.readLine()
	if err != nil {
		return appconfig.Config{}, err
	}
	integrations := current.Integrations
	if selection != "" {
		integrations, err = parseIntegrations(selection)
		if err != nil {
			return appconfig.Config{}, err
		}
	}
	result := appconfig.Config{Version: appconfig.CurrentVersion, Embedding: embedding, Integrations: integrations}
	return result, result.Validate()
}

func (wizard *Wizard) readLine() (string, error) {
	line, err := wizard.input.ReadString('\n')
	if err == io.EOF {
		return strings.TrimSpace(line), nil
	}
	return strings.TrimSpace(line), err
}

func checkbox(enabled bool) string {
	if enabled {
		return "x"
	}
	return " "
}

func selectionLabel(integrations []string) string {
	labels := make([]string, 0, len(integrations))
	for _, integration := range integrations {
		if integration == "codex" {
			labels = append(labels, "1")
		} else if integration == "claude" {
			labels = append(labels, "2")
		}
	}
	return strings.Join(labels, ",")
}

func parseIntegrations(value string) ([]string, error) {
	var result []string
	seen := map[string]bool{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return r == ',' || r == ' ' }) {
		name := ""
		switch raw {
		case "1", "codex":
			name = "codex"
		case "2", "claude", "claude-code":
			name = "claude"
		default:
			return nil, fmt.Errorf("unknown integration selection %q", raw)
		}
		if !seen[name] {
			result = append(result, name)
			seen[name] = true
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("select at least one integration")
	}
	return result, nil
}
