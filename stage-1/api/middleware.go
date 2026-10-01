package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"tablekeeper/contracts"
	"tablekeeper/store"
)

type contextKey string

const (
	UserContextKey contextKey = "user"
)

func WriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

// AuthMiddleware enforces Bearer token authentication
func AuthMiddleware(s *store.Store, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "missing or malformed authorization header")
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "empty bearer token")
			return
		}

		user, err := s.GetUserByToken(r.Context(), token)
		if err != nil || user == nil {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "invalid or unknown token")
			return
		}

		ctx := context.WithValue(r.Context(), UserContextKey, user)
		next(w, r.WithContext(ctx))
	}
}

// GetUserFromContext retrieves authenticated user
func GetUserFromContext(ctx context.Context) *contracts.User {
	if u, ok := ctx.Value(UserContextKey).(*contracts.User); ok {
		return u
	}
	return nil
}

type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       bytes.Buffer
}

func (rec *responseRecorder) WriteHeader(code int) {
	rec.statusCode = code
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *responseRecorder) Write(b []byte) (int, error) {
	rec.body.Write(b)
	return rec.ResponseWriter.Write(b)
}

var idempotencyMu sync.Mutex

// IdempotencyMiddleware guarantees atomic single execution and idempotent replay
func IdempotencyMiddleware(s *store.Store, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			WriteError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
			return
		}
		if len(key) < 1 || len(key) > 255 {
			WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "Idempotency-Key must be 1 to 255 characters")
			return
		}

		user := GetUserFromContext(r.Context())
		if user == nil {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication required for idempotent operation")
			return
		}

		// Read and buffer body
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			WriteError(w, http.StatusBadRequest, "malformed_request", "failed to read body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		// Check if body is valid JSON object
		var jsonRaw interface{}
		if err := json.Unmarshal(bodyBytes, &jsonRaw); err != nil {
			WriteError(w, http.StatusBadRequest, "malformed_request", "invalid JSON body")
			return
		}
		// Canonical normalized JSON for hash comparison
		canonicalBytes, _ := json.Marshal(jsonRaw)
		bodyHash := store.HashBody(canonicalBytes)

		idempotencyMu.Lock()
		// Check existing record
		existing, err := s.GetIdempotency(r.Context(), user.ID, key, r.Method, r.URL.Path)
		if err == nil && existing != nil {
			idempotencyMu.Unlock()
			if existing.BodyHash != bodyHash {
				WriteError(w, http.StatusConflict, "idempotency_key_reuse", "idempotency key reused with different request payload")
				return
			}
			// Replay original response verbatim with HTTP 200
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(existing.ResponseBody))
			return
		}

		rec := &responseRecorder{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		next(rec, r)

		// Cache successful write responses (200, 201)
		if rec.statusCode >= 200 && rec.statusCode < 300 {
			_ = s.SaveIdempotency(r.Context(), &contracts.IdempotencyItem{
				UserID:       user.ID,
				Key:          key,
				Method:       r.Method,
				Path:         r.URL.Path,
				BodyHash:     bodyHash,
				StatusCode:   rec.statusCode,
				ResponseBody: rec.body.String(),
			})
		}
		idempotencyMu.Unlock()
	}
}
