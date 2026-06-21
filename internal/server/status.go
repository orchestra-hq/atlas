package server

import (
	"encoding/json"
	"net/http"
)

// FleetStatus is the one-shot snapshot `atlas status` renders (M2 phase 1): the
// connected workers, the model deployments and their placement state, and the
// headline request/token metrics — the terminal stand-in for the deferred web
// console (build plan decision 2).
type FleetStatus struct {
	Workers     []WorkerInfo     `json:"workers"`
	Deployments []DeploymentInfo `json:"deployments"`
	Metrics     MetricsSnapshot  `json:"metrics"`
}

// StatusHandler builds the GET /admin/status handler from the live control-plane
// objects. workers and deployments are read fresh per request; m may be nil
// (metering off), in which case the metrics headline is zero. The admin surface
// is Atlas's own control plane, so the response is a plain JSON object, not the
// Anthropic envelope (matching the rest of /admin/*).
func StatusHandler(workers func() []WorkerInfo, deployments func() []DeploymentInfo, m *Metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		status := FleetStatus{
			Workers:     workers(),
			Deployments: deployments(),
			Metrics:     m.Snapshot(),
		}
		// Encode empty (not null) collections so the CLI always decodes a slice.
		if status.Workers == nil {
			status.Workers = []WorkerInfo{}
		}
		if status.Deployments == nil {
			status.Deployments = []DeploymentInfo{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	}
}
