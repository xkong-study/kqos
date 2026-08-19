package usage

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/kongxiangrui/kqos/pkg/metrics"
)

// ReportPath is the endpoint agents push to.
const ReportPath = "/v1/usage"

// maxReportBytes caps a single report body. An agent on a 300-pod node sends
// roughly 60KB, so 4MB is generous while still refusing a malformed or hostile
// body outright rather than buffering it.
const maxReportBytes = 4 << 20

// Handler serves the ingestion endpoint.
func Handler(store *Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(ReportPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxReportBytes))
		if err != nil {
			metrics.UsageReportsTotal.WithLabelValues("receive", "read-error").Inc()
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var report Report
		if err := json.Unmarshal(body, &report); err != nil {
			metrics.UsageReportsTotal.WithLabelValues("receive", "decode-error").Inc()
			http.Error(w, "decode report: "+err.Error(), http.StatusBadRequest)
			return
		}
		if report.Node == "" {
			metrics.UsageReportsTotal.WithLabelValues("receive", "no-node").Inc()
			http.Error(w, "report is missing node name", http.StatusBadRequest)
			return
		}
		accepted := store.Ingest(report)
		metrics.UsageReportsTotal.WithLabelValues("receive", "ok").Inc()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": accepted,
			"node":     report.Node,
		})
	})

	// A trivially cheap liveness path so the agent can tell "controller is
	// down" from "controller rejected my payload" without parsing errors.
	mux.HandleFunc("/v1/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return mux
}
