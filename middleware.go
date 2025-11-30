package vital

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Middleware is a function that wraps an http.Handler.
type Middleware func(http.Handler) http.Handler

// W3C Trace Context constants for validation and defaults.
const (
	traceVersion          = "00"
	traceIDLength         = 32
	spanIDLength          = 16
	traceFlagsLength      = 2
	traceparentHeaderName = "Traceparent"
	tracestateHeaderName  = "Tracestate"
	traceFlagSampled      = "01"
	traceFlagNotSampled   = "00"
)

// traceContext represents a W3C Trace Context with traceparent and optional tracestate.
type traceContext struct {
	Version    string // Always "00"
	TraceID    string // 32 hex characters
	SpanID     string // 16 hex characters (current span)
	TraceFlags string // 2 hex characters
	TraceState string // Optional, comma-separated key=value pairs
}

// FormatTraceparent returns the traceparent header value in W3C format.
func (tc *traceContext) FormatTraceparent() string {
	return fmt.Sprintf("%s-%s-%s-%s", tc.Version, tc.TraceID, tc.SpanID, tc.TraceFlags)
}

// BasicAuth returns a middleware that requires HTTP Basic Authentication.
// It uses constant-time comparison to prevent timing attacks.
func BasicAuth(username, password string, realm string) Middleware {
	if realm == "" {
		realm = "Restricted"
	}

	// Pre-hash the credentials for constant-time comparison
	hashedUsername := sha256.Sum256([]byte(username))
	hashedPassword := sha256.Sum256([]byte(password))

	return func(next http.Handler) http.Handler {
		//nolint:varnamelen // w and r are conventional names for http.ResponseWriter and *http.Request
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			//nolint:varnamelen // ok is conventional for boolean return values
			providedUsername, providedPassword, ok := r.BasicAuth()

			// Hash provided credentials
			hashedProvidedUsername := sha256.Sum256([]byte(providedUsername))
			hashedProvidedPassword := sha256.Sum256([]byte(providedPassword))

			// Use constant-time comparison to prevent timing attacks
			usernameMatch := subtle.ConstantTimeCompare(hashedUsername[:], hashedProvidedUsername[:]) == 1
			passwordMatch := subtle.ConstantTimeCompare(hashedPassword[:], hashedProvidedPassword[:]) == 1

			if !ok || !usernameMatch || !passwordMatch {
				w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
				RespondProblem(w, Unauthorized("authentication required"))

				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequestLogger returns a middleware that logs HTTP requests and responses.
// It logs the method, path, status code, duration, and remote address.
func RequestLogger(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		//nolint:varnamelen // w and r are conventional names for http.ResponseWriter and *http.Request
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap the ResponseWriter to capture the status code
			wrapped := &responseWriter{
				ResponseWriter: w,
				statusCode:     http.StatusOK,
			}

			// Call the next handler
			next.ServeHTTP(wrapped, r)

			duration := time.Since(start)

			// Log the request with context (trace context will be added automatically)
			logger.InfoContext(
				r.Context(),
				"http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", wrapped.statusCode),
				slog.Duration("duration", duration),
				slog.String("remote_addr", r.RemoteAddr),
				slog.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter

	statusCode int
}

// WriteHeader captures the status code and calls the underlying WriteHeader.
func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// TraceContext returns a middleware that implements W3C Trace Context propagation.
// It parses incoming traceparent/tracestate headers, generates a new child span,
// and propagates the trace context in response headers and request context.
//
// Behavior:
//   - If valid traceparent exists: Parse it, generate new child span-id
//   - If no/invalid traceparent: Generate new trace-id and span-id
//   - Always sets traceparent and tracestate (if present) in response headers
//   - Adds trace_id, span_id, trace_flags to request context for logging
func TraceContext() Middleware {
	return func(next http.Handler) http.Handler {
		//nolint:varnamelen // w and r are conventional names for http.ResponseWriter and *http.Request
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Parse incoming trace context from headers
			traceparent := r.Header.Get(traceparentHeaderName)
			tracestate := r.Header.Get(tracestateHeaderName)

			var tc *traceContext

			if traceparent != "" {
				// Try to parse existing traceparent
				parsed, err := parseTraceparent(traceparent)
				if err == nil && parsed != nil {
					// Valid traceparent: generate new child span
					tc = &traceContext{
						Version:    parsed.Version,
						TraceID:    parsed.TraceID,
						SpanID:     generateSpanID(),
						TraceFlags: parsed.TraceFlags,
						TraceState: tracestate,
					}
				}
			}

			if tc == nil {
				// No valid traceparent: generate new trace
				tc = generateTraceContext()
			}

			// Add trace context to request context
			ctx := r.Context()
			ctx = context.WithValue(ctx, TraceIDKey, tc.TraceID)
			ctx = context.WithValue(ctx, SpanIDKey, tc.SpanID)
			ctx = context.WithValue(ctx, TraceFlagsKey, tc.TraceFlags)
			r = r.WithContext(ctx)

			// Set response headers
			w.Header().Set(traceparentHeaderName, tc.FormatTraceparent())

			if tc.TraceState != "" {
				w.Header().Set(tracestateHeaderName, tc.TraceState)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// parseTraceparent parses and validates a traceparent header value.
// Returns nil, error if invalid.
//
//nolint:cyclop,err113 // Validation requires multiple checks; dynamic errors provide context
func parseTraceparent(traceparent string) (*traceContext, error) {
	parts := strings.Split(traceparent, "-")
	//nolint:mnd // W3C spec defines 4 parts: version-trace-id-span-id-flags
	if len(parts) != 4 {
		return nil, fmt.Errorf("invalid traceparent format: expected 4 parts, got %d", len(parts))
	}

	version := parts[0]
	traceID := parts[1]
	spanID := parts[2]
	traceFlags := parts[3]

	// Validate version
	if version != traceVersion {
		return nil, fmt.Errorf("unsupported traceparent version: %s", version)
	}

	// Validate trace-id
	if len(traceID) != traceIDLength {
		return nil, fmt.Errorf("invalid trace-id length: expected %d, got %d", traceIDLength, len(traceID))
	}

	if !isValidHex(traceID) {
		return nil, errors.New("invalid trace-id: not valid hex")
	}

	if traceID == "00000000000000000000000000000000" {
		return nil, errors.New("invalid trace-id: all zeros")
	}

	// Validate span-id
	if len(spanID) != spanIDLength {
		return nil, fmt.Errorf("invalid span-id length: expected %d, got %d", spanIDLength, len(spanID))
	}

	if !isValidHex(spanID) {
		return nil, errors.New("invalid span-id: not valid hex")
	}

	if spanID == "0000000000000000" {
		return nil, errors.New("invalid span-id: all zeros")
	}

	// Validate trace-flags
	if len(traceFlags) != traceFlagsLength {
		return nil, fmt.Errorf("invalid trace-flags length: expected %d, got %d", traceFlagsLength, len(traceFlags))
	}

	if !isValidHex(traceFlags) {
		return nil, errors.New("invalid trace-flags: not valid hex")
	}

	return &traceContext{
		Version:    version,
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: traceFlags,
		TraceState: "",
	}, nil
}

// isValidHex checks if a string contains only valid hexadecimal characters.
func isValidHex(s string) bool {
	for _, c := range s {
		//nolint:staticcheck // Current form is more readable than De Morgan's law
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}

	return true
}

// generateTraceContext generates a new W3C Trace Context with random trace-id and span-id.
func generateTraceContext() *traceContext {
	return &traceContext{
		Version:    traceVersion,
		TraceID:    generateTraceID(),
		SpanID:     generateSpanID(),
		TraceFlags: traceFlagSampled,
		TraceState: "",
	}
}

// generateTraceID generates a cryptographically secure random trace ID (16 bytes = 32 hex chars).
func generateTraceID() string {
	const traceIDBytes = 16

	bytes := make([]byte, traceIDBytes)

	_, err := rand.Read(bytes)
	if err != nil {
		// Fallback: use timestamp + random partial bytes
		timestamp := time.Now().UnixNano()
		//nolint:gosec // Timestamp conversion is safe within int64 range
		binary.BigEndian.PutUint64(bytes[:8], uint64(timestamp))
		// Fill remaining with pseudo-random
		//nolint:mnd // Bit shift by 8 for byte extraction
		for i := 8; i < traceIDBytes; i++ {
			bytes[i] = byte(timestamp >> (i * 8))
		}
	}

	return hex.EncodeToString(bytes)
}

// generateSpanID generates a cryptographically secure random span ID (8 bytes = 16 hex chars).
func generateSpanID() string {
	const spanIDBytes = 8

	bytes := make([]byte, spanIDBytes)

	_, err := rand.Read(bytes)
	if err != nil {
		// Fallback: use timestamp
		timestamp := time.Now().UnixNano()
		//nolint:gosec // Timestamp conversion is safe within int64 range
		binary.BigEndian.PutUint64(bytes, uint64(timestamp))
	}

	return hex.EncodeToString(bytes)
}

// GetTraceID retrieves the trace ID from the request context.
func GetTraceID(ctx context.Context) string {
	if traceID, ok := ctx.Value(TraceIDKey).(string); ok {
		return traceID
	}

	return ""
}

// GetSpanID retrieves the span ID from the request context.
func GetSpanID(ctx context.Context) string {
	if spanID, ok := ctx.Value(SpanIDKey).(string); ok {
		return spanID
	}

	return ""
}

// GetTraceFlags retrieves the trace flags from the request context.
func GetTraceFlags(ctx context.Context) string {
	if traceFlags, ok := ctx.Value(TraceFlagsKey).(string); ok {
		return traceFlags
	}

	return ""
}

// Recovery returns a middleware that recovers from panics and returns a 500 error.
func Recovery(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		//nolint:varnamelen // w and r are conventional names for http.ResponseWriter and *http.Request
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					logger.Error(
						"panic recovered",
						slog.Any("error", err),
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
					)

					RespondProblem(w, InternalServerError("internal server error"))
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}
