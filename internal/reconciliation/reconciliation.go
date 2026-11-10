package reconciliation

import (
	"context"

	"ragfy/internal/knowledge"
)

type Relation string

const (
	RelationDuplicate  Relation = "duplicate"
	RelationSupersedes Relation = "supersedes"
)

type Request struct {
	SessionID string
	Provider  string
	Project   string
	New       []knowledge.Document
	Existing  []knowledge.Document
}

type Action struct {
	NewSourceID      string
	ExistingSourceID string
	Relation         Relation
}

type Reconciler interface {
	Reconcile(context.Context, Request) ([]Action, error)
}

type None struct{}

func (None) Reconcile(context.Context, Request) ([]Action, error) { return nil, nil }
