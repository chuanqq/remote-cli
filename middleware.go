package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
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
