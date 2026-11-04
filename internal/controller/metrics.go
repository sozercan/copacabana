package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	buildsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "copa_builds_total",
		Help: "PatchBuilds by final phase.",
	}, []string{"phase"})
	rolloutsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "copa_rollouts_total",
		Help: "PatchRollouts by final phase.",
	}, []string{"phase"})
	rolloutDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "copa_rollout_to_running_seconds",
		Help:    "Time from a PatchRollout being created to the candidate being known-good.",
		Buckets: prometheus.ExponentialBuckets(30, 2, 12),
	})
	missedSwaps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "copa_missed_swaps_total",
		Help: "Opted-in pods created with a swappable image that admission didn't swap.",
	}, []string{"namespace"})
)

func init() {
	metrics.Registry.MustRegister(buildsTotal, rolloutsTotal, rolloutDuration, missedSwaps)
}
