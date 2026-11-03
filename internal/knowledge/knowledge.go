package knowledge

import "context"

type Document struct {
	Source   string         `json:"source"`
	SourceID string         `json:"source_id"`
	TS       string         `json:"ts"`
	Title    string         `json:"title"`
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata"`
	Kind     string         `json:"kind"`
	Area     string         `json:"area"`
	Vector   []float32      `json:"-"`
}

type Hit struct {
	Score float64
	Doc   Document
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
