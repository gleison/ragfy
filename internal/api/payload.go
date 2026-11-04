package api

type HookPayload struct {
	Source        string `json:"source"`
	Prompt        string `json:"prompt"`
	SessionID     string `json:"session_id"`
	Transcript    string `json:"transcript_path"`
	CWD           string `json:"cwd"`
	Event         string `json:"hook_event_name"`
	EventCodex    string `json:"event"`
	LastAssistant string `json:"last_assistant_message"`
}

type IngestPayload struct {
	Path    string `json:"path"`
	Project string `json:"project,omitempty"`
}
