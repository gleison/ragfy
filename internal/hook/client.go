package hook

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ragfy/internal/api"
)

func Run(action, source, dataDirectory, listenAddress string, input io.Reader, output io.Writer) {
	timeout := 4 * time.Second
	if action == "digest" {
		timeout = 2 * time.Second
	}
	run(action, source, dataDirectory, listenAddress, input, output, &http.Client{Timeout: timeout})
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func run(action, source, dataDirectory, listenAddress string, input io.Reader, output io.Writer, client httpDoer) {
	if os.Getenv("RAGFY_AGENT_EXEC") != "" {
		return
	}
	payloadBytes, _ := io.ReadAll(io.LimitReader(input, 4<<20))
	var payload api.HookPayload
	if json.Unmarshal(payloadBytes, &payload) != nil {
		return
	}
	payload.Source = source
	payloadBytes, _ = json.Marshal(payload)
	endpoint := "/recall"
	eventName := "UserPromptSubmit"
	if action == "bootstrap" || strings.EqualFold(payload.Event, "SessionStart") || strings.EqualFold(payload.EventCodex, "SessionStart") {
		endpoint = "/bootstrap"
		eventName = "SessionStart"
	} else if action == "digest" || strings.EqualFold(payload.Event, "SessionEnd") || strings.EqualFold(payload.EventCodex, "SessionEnd") {
		endpoint = "/digest"
	}
	token, _ := os.ReadFile(filepath.Join(dataDirectory, "token"))
	request, _ := http.NewRequest(http.MethodPost, "http://"+listenAddress+endpoint, strings.NewReader(string(payloadBytes)))
	if request == nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	if endpoint == "/digest" {
		return
	}
	var result api.RecallResponse
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result) == nil {
		_ = WriteContextOutput(output, eventName, result.Context)
	}
}

func WritePromptOutput(writer io.Writer, contextText string) error {
	return WriteContextOutput(writer, "UserPromptSubmit", contextText)
}

func WriteContextOutput(writer io.Writer, eventName, contextText string) error {
	if strings.TrimSpace(contextText) == "" {
		return nil
	}
	return json.NewEncoder(writer).Encode(map[string]any{
		"hookSpecificOutput": map[string]string{
			"hookEventName":     eventName,
			"additionalContext": contextText,
		},
	})
}
