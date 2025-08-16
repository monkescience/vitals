package vitals

import (
	"context"
	"encoding/json"
	"log/slog"
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
	Name     string `json:"name"`
	Status   Status `json:"status"`
	Message  string `json:"message,omitempty"`
	Duration string `json:"duration,omitempty"`
}

type Checker interface {
	Name() string
	Check(ctx context.Context) (Status, string)
}

type readyConfig struct {
	overallTimeout  time.Duration
	perCheckTimeout time.Duration
}

type ReadyOption func(*readyConfig)

func WithOverallReadyTimeout(d time.Duration) ReadyOption {
	return func(c *readyConfig) { c.overallTimeout = d }
}

func WithPerCheckTimeout(d time.Duration) ReadyOption {
	return func(c *readyConfig) { c.perCheckTimeout = d }
}

func NewHandler(version string, environment string, checkers []Checker, opts ...ReadyOption) http.Handler {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}

	startTime := time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", LiveHandlerFunc(version, startTime, host, environment))
	mux.HandleFunc("GET /health/ready", ReadyHandlerFunc(checkers, opts...))

	return mux
}

// LiveHandlerFunc handles liveness check requests
func LiveHandlerFunc(version string, startTime time.Time, host string, environment string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
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
		err := json.NewEncoder(w).Encode(response)
		if err != nil {
			slog.ErrorContext(
				ctx,
				"failed to encode live health response",
				slog.String("handler", "live"),
				slog.String("route", "/health/live"),
				slog.Int("status", http.StatusOK),
				slog.Any("error", err),
			)
		}
	}
}

// ReadyHandlerFunc handles readiness check requests with context and timeouts
func ReadyHandlerFunc(checkers []Checker, opts ...ReadyOption) http.HandlerFunc {
	cfg := readyConfig{
		overallTimeout:  2 * time.Second,
		perCheckTimeout: 800 * time.Millisecond,
	}
	for _, o := range opts {
		o(&cfg)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if cfg.overallTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, cfg.overallTimeout)
			defer cancel()
		}

		response := ReadyResponse{
			Status: StatusOK,
			Checks: make([]CheckResponse, len(checkers)),
		}

		var wg sync.WaitGroup
		wg.Add(len(checkers))

		for idx, checker := range checkers {
			i, c := idx, checker
			go func() {
				defer wg.Done()
				start := time.Now()
				cctx := ctx
				if cfg.perCheckTimeout > 0 {
					var ccancel context.CancelFunc
					cctx, ccancel = context.WithTimeout(ctx, cfg.perCheckTimeout)
					defer ccancel()
				}

				status, msg := c.Check(cctx)
				if err := cctx.Err(); err != nil && status == StatusOK {
					status = StatusError
					if msg == "" {
						msg = err.Error()
					} else {
						msg = msg + "; " + err.Error()
					}
				}

				response.Checks[i] = CheckResponse{
					Name:     c.Name(),
					Status:   status,
					Message:  msg,
					Duration: time.Since(start).String(),
				}
			}()
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
		statusCode := http.StatusOK
		if response.Status != StatusOK {
			statusCode = http.StatusServiceUnavailable
		}
		w.WriteHeader(statusCode)

		err := json.NewEncoder(w).Encode(response)
		if err != nil {
			slog.ErrorContext(
				ctx,
				"failed to encode ready health response",
				slog.String("handler", "ready"),
				slog.String("route", "/health/ready"),
				slog.Int("status", statusCode),
				slog.Any("error", err),
			)
		}
	}
}

func disableResponseCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "Thu, 01 Jan 1970 00:00:00 GMT")
}
