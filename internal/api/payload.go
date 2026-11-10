package api

type HookPayload struct {
	Source        string `json:"source"`
	Prompt        string `json:"prompt"`
	SessionID     string `json:"session_id"`
	Transcript    string `json:"transcript_path"`
	CWD           string `json:"cwd"`
	Project       string `json:"project,omitempty"`
	Event         string `json:"hook_event_name"`
	EventCodex    string `json:"event"`
	LastAssistant string `json:"last_assistant_message"`
}

type RecallResponse struct {
	Context string `json:"context"`
}

type IngestPayload struct {
	Path     string `json:"path"`
	Text     string `json:"text,omitempty"`
	Title    string `json:"title,omitempty"`
	Project  string `json:"project,omitempty"`
	Provider string `json:"provider,omitempty"`
}
