package model

import "context"

type Embedder interface {
	Embed(context.Context, []string) ([][]float32, error)
	EmbedQuery(context.Context, string) ([]float32, error)
}

type Generator interface {
	Generate(context.Context, string) (string, error)
}
