package operator

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type InjectableController interface {
	Controller

	InjectSettings(context.Context) context.Context
}

type SettingsDecorator struct {
	c InjectableController
}

func (sd *SettingsDecorator) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = sd.c.InjectSettings(ctx)
	return sd.c.Reconcile(ctx, req)
}

// Controller is an interface implemented by Karpenter custom resources.
type Controller interface {
	// Reconcile hands a hydrated kubernetes resource to the controller for
	// reconciliation. Any changes made to the resource's status are persisted
	// after Reconcile returns, even if it returns an error.
	Reconcile(context.Context, reconcile.Request) (reconcile.Result, error)
	// Register will register the controller with the manager
	Register(context.Context, manager.Manager) error
}
