package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s %v", r.RemoteAddr, r.Method, r.URL.Path, time.Since(start))
	})
}

type RateLimiter struct {
	mu        sync.Mutex
	tokens    map[string]*bucket
	maxPerMin int
}

type bucket struct {
	tokens    int
	lastReset time.Time
}

func NewRateLimiter(maxPerMin int) *RateLimiter {
	return &RateLimiter{
		tokens:    make(map[string]*bucket),
		maxPerMin: maxPerMin,
	}
}

func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, ok := rl.tokens[key]
	if !ok {
		rl.tokens[key] = &bucket{tokens: rl.maxPerMin - 1, lastReset: now}
		return true
	}

	if now.Sub(b.lastReset) >= time.Minute {
		b.tokens = rl.maxPerMin - 1
		b.lastReset = now
		return true
	}

	if b.tokens <= 0 {
		return false
	}

	b.tokens--
	return true
}

func (rl *RateLimiter) Remaining(key string) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.tokens[key]
	if !ok {
		return rl.maxPerMin
	}
	if time.Since(b.lastReset) >= time.Minute {
		return rl.maxPerMin
	}
	return b.tokens
}

func RateLimitMiddleware(rl *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			next.ServeHTTP(w, r)
			return
		}

		key := r.RemoteAddr
		if !rl.Allow(key) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "Too many requests")
			return
		}

		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rl.maxPerMin))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(rl.Remaining(key)))

		next.ServeHTTP(w, r)
	})
}

// ReadOnlyMiddleware is layer 3 of read-only enforcement: a blanket method
// filter over the REST surface. Only GET/HEAD survive under /api/, so a
// mutating endpoint added later is denied before its handler is reached.
//
// /mcp is exempt because MCP rides on POST by protocol; its calls are gated by
// tool registration (layer 1) plus the operation guards (layer 4).
func ReadOnlyMiddleware(readOnly bool, next http.Handler) http.Handler {
	if !readOnly {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") &&
			r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("X-Read-Only", "true")
			writeError(w, http.StatusForbidden, "read_only_mode",
				"Server is in read-only mode: "+r.Method+" "+r.URL.Path+" is disabled")
			return
		}
		w.Header().Set("X-Read-Only", "true")
		next.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ErrorResponse{
		Error:   errType,
		Message: message,
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
