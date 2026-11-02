package httpx

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
)

var processStartTime = time.Now()

type jobCounter interface {
	CountByOperationStatus(context.Context) ([]jobs.JobCount, error)
}

func (a *App) ConfigureMetrics(token string, disabled bool, counts jobCounter, admission *inference.Admission) {
	a.metricsToken = token
	a.metricsAuthDisabled = disabled
	a.metricsJobs = counts
	a.metricsAdmission = admission
}

func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if a.metricsToken != "" {
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		wantHash := sha256.Sum256([]byte(a.metricsToken))
		gotHash := sha256.Sum256([]byte(strings.TrimPrefix(header, prefix)))
		if !strings.HasPrefix(header, prefix) || subtle.ConstantTimeCompare(wantHash[:], gotHash[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	} else if !a.metricsAuthDisabled {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var counts []jobs.JobCount
	if a.metricsJobs != nil {
		var err error
		counts, err = a.metricsJobs.CountByOperationStatus(r.Context())
		if err != nil {
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	lines := []string{
		"# HELP enclava_jobs Jobs by operation and status.",
		"# TYPE enclava_jobs gauge",
	}
	for _, c := range counts {
		lines = append(lines, fmt.Sprintf("enclava_jobs{operation=%q,status=%q} %d", c.Operation, c.Status, c.Count))
	}
	if a.metricsAdmission != nil {
		lines = append(lines,
			"# HELP enclava_inference_active Active inference requests by admission pool.",
			"# TYPE enclava_inference_active gauge",
			"# HELP enclava_inference_queued Queued inference requests by admission pool.",
			"# TYPE enclava_inference_queued gauge",
		)
		for _, pool := range []struct {
			name  string
			class inference.AdmissionClass
		}{{"execute", inference.AdmissionExecute}, {"manage", inference.AdmissionManage}, {"fanout", inference.AdmissionFanOut}} {
			active, queued, _ := a.metricsAdmission.Stats(pool.class)
			lines = append(lines,
				fmt.Sprintf("enclava_inference_active{pool=%q} %d", pool.name, active),
				fmt.Sprintf("enclava_inference_queued{pool=%q} %d", pool.name, queued),
			)
		}
	}
	lines = append(lines,
		"# HELP go_goroutines Number of goroutines.",
		"# TYPE go_goroutines gauge",
		fmt.Sprintf("go_goroutines %d", runtime.NumGoroutine()),
		"# HELP process_start_time_seconds Start time of the process since the Unix epoch.",
		"# TYPE process_start_time_seconds gauge",
		fmt.Sprintf("process_start_time_seconds %g", float64(processStartTime.UnixNano())/1e9),
	)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := w.Write([]byte(strings.Join(lines, "\n") + "\n")); err != nil {
		return
	}
}
