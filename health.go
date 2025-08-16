package vitals

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
)

type Status string

const (
	StatusOK    Status = "ok"
	StatusError Status = "error"
)

type LiveResponse struct {
	Status      Status `json:"status"`
	Version     string `json:"version"`
	Uptime      string `json:"uptime"`
	GoVersion   string `json:"go_version"`
	Host        string `json:"host"`
	Environment string `json:"environment"`
}

// ReadyResponse represents the readiness check response
type ReadyResponse struct {
	Status Status          `json:"status"`
	Checks []CheckResponse `json:"checks"`
}

type CheckResponse struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
}

type Checker interface {
	Name() string
	Check() Status
}

func NewHandler(version string, environment string, checkers []Checker) http.Handler {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}

	startTime := time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", LiveHandlerFunc(version, startTime, host, environment))
	mux.HandleFunc("GET /health/ready", ReadyHandlerFunc(checkers))

	return mux
}

// LiveHandlerFunc handles liveness check requests
func LiveHandlerFunc(version string, startTime time.Time, host string, environment string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		response := LiveResponse{
			Status:      StatusOK,
			Version:     version,
			Uptime:      time.Since(startTime).String(),
			GoVersion:   runtime.Version(),
			Host:        host,
			Environment: environment,
		}

		disableResponseCacheHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	}
}

// ReadyHandlerFunc handles readiness check requests
func ReadyHandlerFunc(checkers []Checker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		response := ReadyResponse{
			Status: StatusOK,
			Checks: make([]CheckResponse, len(checkers)),
		}

		var wg sync.WaitGroup
		wg.Add(len(checkers))

		for idx, checker := range checkers {
			go func(i int, c Checker) {
				defer wg.Done()
				status := c.Check()
				response.Checks[i] = CheckResponse{
					Name:   c.Name(),
					Status: status,
				}
			}(idx, checker)
		}

		wg.Wait()

		for _, check := range response.Checks {
			if check.Status != StatusOK {
				response.Status = StatusError
				break
			}
		}

		disableResponseCacheHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		if response.Status != StatusOK {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		_ = json.NewEncoder(w).Encode(response)
	}
}

func disableResponseCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "Thu, 01 Jan 1970 00:00:00 GMT")
}
