package controller

import (
	"time"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func controllerOptions(workers int) controller.Options {
	return controller.Options{
		MaxConcurrentReconciles: workers,
		RateLimiter:             workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](time.Second, time.Minute),
	}
}
