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

func Run(action, dataDirectory, listenAddress string, input io.Reader, output io.Writer) {
	payloadBytes, _ := io.ReadAll(io.LimitReader(input, 4<<20))
	var payload api.HookPayload
	if json.Unmarshal(payloadBytes, &payload) != nil {
		return
	}
	endpoint := "/recall"
	if action == "digest" || strings.EqualFold(payload.Event, "SessionEnd") || strings.EqualFold(payload.EventCodex, "SessionEnd") {
		endpoint = "/digest"
	}
	token, _ := os.ReadFile(filepath.Join(dataDirectory, "token"))
	request, _ := http.NewRequest(http.MethodPost, "http://"+listenAddress+endpoint, strings.NewReader(string(payloadBytes)))
	if request == nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Content-Type", "application/json")
	timeout := 4 * time.Second
	if endpoint == "/digest" {
		timeout = 2 * time.Second
	}
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	if endpoint != "/recall" {
		return
	}
	var result struct {
		Context string `json:"context"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result) == nil {
		_ = WritePromptOutput(output, result.Context)
	}
}

func WritePromptOutput(writer io.Writer, contextText string) error {
	if strings.TrimSpace(contextText) == "" {
		return nil
	}
	return json.NewEncoder(writer).Encode(map[string]any{
		"hookSpecificOutput": map[string]string{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": contextText,
		},
	})
}
