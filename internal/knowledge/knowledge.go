package knowledge

import "context"

type Scope string

const (
	ScopeGlobal  Scope = "global"
	ScopeProject Scope = "project"
)

func (scope Scope) Valid() bool {
	return scope == ScopeGlobal || scope == ScopeProject
}

type Document struct {
	Source   string         `json:"source"`
	SourceID string         `json:"source_id"`
	TS       string         `json:"ts"`
	Title    string         `json:"title"`
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata"`
	Kind     string         `json:"kind"`
	Area     string         `json:"area"`
	Scope    Scope          `json:"scope"`
	Project  string         `json:"project,omitempty"`
	Vector   []float32      `json:"-"`
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

type Repository interface {
	Upsert(context.Context, Document) error
	EmbeddingModel(context.Context) (string, error)
	SetEmbeddingModel(context.Context, string) error
	Documents(context.Context) ([]Document, error)
	UpdateEmbedding(context.Context, string, string, []float32) error
	HasSession(context.Context, string, string) (bool, error)
	MarkSession(context.Context, string, string) (bool, error)
	DeleteSessionDocs(context.Context, string) error
}
