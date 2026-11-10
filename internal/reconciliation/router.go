package reconciliation

import (
	"context"
	"fmt"
)

type Router struct {
	reconcilers map[string]Reconciler
}

var _ Reconciler = (*Router)(nil)

func NewRouter(reconcilers map[string]Reconciler) *Router {
	configured := make(map[string]Reconciler, len(reconcilers))
	for source, reconciler := range reconcilers {
		configured[source] = reconciler
	}
	return &Router{reconcilers: configured}
}

func (router *Router) Reconcile(ctx context.Context, request Request) ([]Action, error) {
	reconciler, ok := router.reconcilers[request.Provider]
	if !ok || reconciler == nil {
		return nil, fmt.Errorf("no knowledge reconciler configured for provider %q", request.Provider)
	}
	return reconciler.Reconcile(ctx, request)
}
