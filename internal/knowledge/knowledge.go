package knowledge

import (
	"context"
	"errors"
)

var ErrNotFound = errors.New("not found")

type Scope string

type Status string

const (
	ScopeGlobal      Scope  = "global"
	ScopeProject     Scope  = "project"
	StatusActive     Status = "active"
	StatusSuperseded Status = "superseded"
)

func (scope Scope) Valid() bool {
	return scope == ScopeGlobal || scope == ScopeProject
}

type Document struct {
	Source              string         `json:"source"`
	SourceID            string         `json:"source_id"`
	TS                  string         `json:"ts"`
	Title               string         `json:"title"`
	Content             string         `json:"content"`
	Metadata            map[string]any `json:"metadata"`
	Kind                string         `json:"kind"`
	Area                string         `json:"area"`
	Scope               Scope          `json:"scope"`
	Project             string         `json:"project,omitempty"`
	Status              Status         `json:"status"`
	SupersededBy        string         `json:"superseded_by,omitempty"`
	SupersededAt        string         `json:"superseded_at,omitempty"`
	ConsolidationReason string         `json:"consolidation_reason,omitempty"`
	OriginID            string         `json:"origin_id,omitempty"`
	Vector              []float32      `json:"-"`
}

func (document Document) HasValidScope() bool {
	return document.Scope == ScopeGlobal && document.Project == "" || document.Scope == ScopeProject && document.Project != ""
}

type Hit struct {
	Score         float64
	SemanticScore float64
	LexicalScore  float64
	ProjectBoost  float64
	Doc           Document
}

type Supersession struct {
	LoserSourceID  string
	WinnerSourceID string
	Reason         string
}

type SessionCommit struct {
	SessionID      string
	TranscriptHash string
	Source         Source
	Documents      []Document
	Supersessions  []Supersession
}

type Source struct {
	ID          string
	Kind        string
	Key         string
	Provider    string
	Project     string
	Title       string
	Content     string
	ContentHash string
	CreatedAt   string
}

type SourceCommit struct {
	Source        Source
	Documents     []Document
	Supersessions []Supersession
}

type Statistics struct {
	ActiveRecords     int `json:"active_records"`
	SupersededRecords int `json:"superseded_records"`
	DigestedSessions  int `json:"digested_sessions"`
	PreservedSources  int `json:"preserved_sources"`
	Projects          int `json:"projects"`
}

type Session struct {
	ID                string     `json:"id"`
	Provider          string     `json:"provider,omitempty"`
	Project           string     `json:"project,omitempty"`
	TranscriptHash    string     `json:"transcript_hash"`
	DigestedAt        string     `json:"digested_at"`
	SourceVersions    int        `json:"source_versions"`
	ActiveRecords     int        `json:"active_records"`
	SupersededRecords int        `json:"superseded_records"`
	Sources           []Source   `json:"sources,omitempty"`
	Documents         []Document `json:"documents,omitempty"`
}

type ForgetResult struct {
	SessionID          string `json:"session_id"`
	RemovedSources     int    `json:"removed_sources"`
	RemovedRecords     int    `json:"removed_records"`
	ReactivatedRecords int    `json:"reactivated_records"`
	RemovedRecalls     int    `json:"removed_recalls"`
}

type RecallResult struct {
	Rank          int     `json:"rank"`
	SelectedRank  int     `json:"selected_rank,omitempty"`
	Status        string  `json:"status"`
	Feedback      string  `json:"feedback,omitempty"`
	SourceID      string  `json:"source_id"`
	Title         string  `json:"title"`
	Content       string  `json:"content"`
	Kind          string  `json:"kind"`
	Scope         Scope   `json:"scope"`
	Project       string  `json:"project,omitempty"`
	Score         float64 `json:"score"`
	SemanticScore float64 `json:"semantic_score"`
	LexicalScore  float64 `json:"lexical_score"`
	ProjectBoost  float64 `json:"project_boost"`
}

type RecallEvent struct {
	ID             string         `json:"id"`
	SessionID      string         `json:"session_id,omitempty"`
	Source         string         `json:"source"`
	Project        string         `json:"project,omitempty"`
	Prompt         string         `json:"prompt"`
	CreatedAt      string         `json:"created_at"`
	ResultCount    int            `json:"result_count"`
	CandidateCount int            `json:"candidate_count"`
	ScoreCutoff    float64        `json:"score_cutoff"`
	LexicalCutoff  float64        `json:"lexical_cutoff"`
	Results        []RecallResult `json:"results,omitempty"`
}

type Repository interface {
	Upsert(context.Context, Document) error
	EmbeddingModel(context.Context) (string, error)
	SetEmbeddingModel(context.Context, string) error
	Documents(context.Context) ([]Document, error)
	UpdateEmbedding(context.Context, string, string, []float32) error
	HasSession(context.Context, string, string) (bool, error)
	CommitSession(context.Context, SessionCommit) error
	HasSource(context.Context, string) (bool, error)
	CommitSource(context.Context, SourceCommit) error
	Sources(context.Context) ([]Source, error)
	Statistics(context.Context) (Statistics, error)
	Sessions(context.Context) ([]Session, error)
	Session(context.Context, string) (Session, error)
	ForgetSession(context.Context, string) (ForgetResult, error)
	RecordRecall(context.Context, RecallEvent) error
	RecallEvents(context.Context) ([]RecallEvent, error)
	RecallEvent(context.Context, string) (RecallEvent, error)
	SetRecallFeedback(context.Context, string, int, string) (RecallResult, error)
}
