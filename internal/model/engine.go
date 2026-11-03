package model

import "context"

type Engine interface {
	Embed(context.Context, []string) ([][]float32, error)
	Generate(context.Context, string) (string, error)
}
