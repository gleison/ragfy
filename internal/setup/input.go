package setup

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	appconfig "ragfy/internal/config"
)

func askConfiguration(input io.Reader, output io.Writer, current appconfig.Config) (appconfig.Config, error) {
	reader := bufio.NewReader(input)
	fmt.Fprintln(output, "Ragfy setup")
	fmt.Fprintln(output)
	fmt.Fprintf(output, "Embedding model URI [%s]: ", current.Embedding.URI)
	uri, err := readLine(reader)
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

	fmt.Fprintln(output, "\nIntegrations (enter a comma-separated selection):")
	fmt.Fprintf(output, "  [%s] 1. Codex\n", checkbox(current.Enabled("codex")))
	fmt.Fprintf(output, "  [%s] 2. Claude Code\n", checkbox(current.Enabled("claude")))
	fmt.Fprintf(output, "Selection [%s]: ", selectionLabel(current.Integrations))
	selection, err := readLine(reader)
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

func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
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
