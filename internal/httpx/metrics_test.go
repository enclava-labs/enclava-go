package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
)

type metricsCounts struct{ err error }

func (m metricsCounts) CountByOperationStatus(context.Context) ([]jobs.JobCount, error) {
	return []jobs.JobCount{{Operation: "test\"op", Status: "queued", Count: 2}}, m.err
}

func TestMetricsAuthenticationAndOutput(t *testing.T) {
	for _, tt := range []struct {
		name, token, header string
		disabled            bool
		want                int
	}{
		{"closed", "", "", false, http.StatusUnauthorized},
		{"wrong", "secret", "Bearer wrong", false, http.StatusUnauthorized},
		{"correct", "secret", "Bearer secret", false, http.StatusOK},
		{"disabled", "", "", true, http.StatusOK},
		{"token takes precedence", "secret", "", true, http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := &App{}
			a.ConfigureMetrics(tt.token, tt.disabled, metricsCounts{}, inference.NewAdmission(inference.AdmissionConfig{}))
			r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			r.Header.Set("Authorization", tt.header)
			w := httptest.NewRecorder()
			a.handleMetrics(w, r)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
			if tt.want == http.StatusOK {
				for _, want := range []string{"enclava_jobs{operation=\"test\\\"op\",status=\"queued\"} 2", "enclava_inference_active", "enclava_inference_queued", "go_goroutines", "process_start_time_seconds"} {
					if !strings.Contains(w.Body.String(), want) {
						t.Errorf("missing %q in %q", want, w.Body.String())
					}
				}
			}
		})
	}
}

func TestMetricsDatabaseFailure(t *testing.T) {
	a := &App{}
	a.ConfigureMetrics("", true, metricsCounts{err: errors.New("offline")}, nil)
	w := httptest.NewRecorder()
	a.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", w.Code)
	}
}
