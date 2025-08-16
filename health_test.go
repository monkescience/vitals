package vitals

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

type mockChecker struct {
	name  string
	stat  Status
	delay time.Duration
}

func (m mockChecker) Name() string { return m.name }
func (m mockChecker) Check(ctx context.Context) (Status, string) {
	if m.delay <= 0 {
		return m.stat, ""
	}
	select {
	case <-time.After(m.delay):
		return m.stat, ""
	case <-ctx.Done():
		return StatusError, ctx.Err().Error()
	}
}

func Test(t *testing.T) {
	t.Run("health live ok", func(t *testing.T) {
		// given
		version := "1.2.3"
		environment := "eu-central-1-dev"

		handlers := NewHandler(version, environment, []Checker{})
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/health/live", nil)

		// when
		handlers.ServeHTTP(rr, req)

		// then
		if rr.Code != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, http.StatusOK)
		}
	})
}

func TestLiveHandler_OK(t *testing.T) {
	version := "1.2.3"
	startTime := time.Now().Add(-2 * time.Second)
	host := "test-host"
	env := "test-env"

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)

	handler := LiveHandlerFunc(version, startTime, host, env)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// Check headers (no-cache + content type)
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("unexpected content-type: %q", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store, no-cache" {
		t.Errorf("unexpected Cache-Control: %q", got)
	}
	if got := rr.Header().Get("Pragma"); got != "no-cache" {
		t.Errorf("unexpected Pragma: %q", got)
	}
	if got := rr.Header().Get("Expires"); got != "Thu, 01 Jan 1970 00:00:00 GMT" {
		t.Errorf("unexpected Expires: %q", got)
	}

	var resp LiveResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Status != StatusOK {
		t.Errorf("expected status %q, got %q", StatusOK, resp.Status)
	}
	if resp.Version != version {
		t.Errorf("expected version %q, got %q", version, resp.Version)
	}
	if resp.GoVersion != runtime.Version() {
		t.Errorf("expected go version %q, got %q", runtime.Version(), resp.GoVersion)
	}
	if resp.Host != host {
		t.Errorf("expected host %q, got %q", host, resp.Host)
	}
	if resp.Environment != env {
		t.Errorf("expected env %q, got %q", env, resp.Environment)
	}
	if resp.Uptime == "" {
		t.Errorf("expected non-empty uptime")
	} else {
		if d, err := time.ParseDuration(resp.Uptime); err != nil {
			t.Errorf("uptime not a valid duration: %v (value=%q)", err, resp.Uptime)
		} else if d <= 0 {
			t.Errorf("expected uptime > 0, got %v", d)
		}
	}
}

func TestReadyHandler_AllOK(t *testing.T) {
	checkers := []Checker{
		mockChecker{name: "db", stat: StatusOK, delay: 20 * time.Millisecond},
		mockChecker{name: "cache", stat: StatusOK, delay: 10 * time.Millisecond},
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)

	handler := ReadyHandlerFunc(checkers)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	// Check headers
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("unexpected content-type: %q", got)
	}

	var resp ReadyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Status != StatusOK {
		t.Errorf("expected overall status %q, got %q", StatusOK, resp.Status)
	}
	if len(resp.Checks) != len(checkers) {
		t.Fatalf("expected %d checks, got %d", len(checkers), len(resp.Checks))
	}

	// Order should match input indices
	for i, c := range checkers {
		if resp.Checks[i].Name != c.Name() {
			t.Errorf("check %d name mismatch: expected %q, got %q", i, c.Name(), resp.Checks[i].Name)
		}
		// Compare expected status from mockChecker
		if mc, ok := c.(mockChecker); ok {
			if resp.Checks[i].Status != mc.stat {
				t.Errorf("check %d status mismatch: expected %q, got %q", i, mc.stat, resp.Checks[i].Status)
			}
		}
	}
}

func TestReadyHandler_WithError(t *testing.T) {
	checkers := []Checker{
		mockChecker{name: "db", stat: StatusOK},
		mockChecker{name: "third-party", stat: StatusError},
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)

	handler := ReadyHandlerFunc(checkers)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, rr.Code)
	}

	var resp ReadyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Status != StatusError {
		t.Errorf("expected overall status %q, got %q", StatusError, resp.Status)
	}
	if len(resp.Checks) != len(checkers) {
		t.Fatalf("expected %d checks, got %d", len(checkers), len(resp.Checks))
	}
	// Ensure at least one error in checks
	foundErr := false
	for _, ch := range resp.Checks {
		if ch.Status == StatusError {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Errorf("expected at least one failing check in response")
	}
}

func TestNewHandler_Routes(t *testing.T) {
	checkers := []Checker{
		mockChecker{name: "db", stat: StatusOK},
	}
	mux := NewHandler("0.0.1", "test", checkers)

	// Test /health/live
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/health/live expected %d, got %d", http.StatusOK, rr.Code)
	}

	// Test /health/ready
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	mux.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("/health/ready expected %d, got %d", http.StatusOK, rr2.Code)
	}
}

func TestReadyHandler_DiagnosticsAndTimeout(t *testing.T) {
	checkers := []Checker{
		mockChecker{name: "fast", stat: StatusOK, delay: 10 * time.Millisecond},
		mockChecker{name: "slow", stat: StatusOK, delay: 2 * time.Second},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	h := ReadyHandlerFunc(checkers, WithPerCheckTimeout(50*time.Millisecond), WithOverallReadyTimeout(1*time.Second))
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	var resp ReadyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != StatusError {
		t.Fatalf("expected error status")
	}
	if len(resp.Checks) != 2 {
		t.Fatalf("expected 2 checks")
	}
	if resp.Checks[0].Duration == "" || resp.Checks[1].Duration == "" {
		t.Fatalf("missing durations")
	}
	if resp.Checks[1].Status != StatusError {
		t.Fatalf("slow should be error due to timeout")
	}
	if resp.Checks[1].Message == "" {
		t.Fatalf("expected timeout message")
	}
}
