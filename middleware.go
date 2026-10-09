package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimiter is a per-host token bucket: capacity `burst`, refilled
// continuously at `perMin` tokens per minute.
//
// The key is the peer HOST, never host:port. Keying on r.RemoteAddr (as an
// earlier version did) gave every new TCP connection a fresh full bucket, so
// the limit never engaged: in production one client sent 85 requests in a
// minute over 84 source ports against a configured 60/min.
//
// A token bucket (rather than a fixed per-minute window) absorbs the bursts a
// legitimate MCP client produces — every reconnect is initialize +
// notifications/initialized + tools/list back to back — while still capping the
// sustained rate.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	perMin  int
	burst   int
	now     func() time.Time // injectable for tests
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// bucketIdleTTL: a bucket untouched this long has refilled to capacity anyway,
// so dropping it loses nothing and keeps the map bounded under port scans.
const bucketIdleTTL = 2 * time.Minute

func NewRateLimiter(perMin, burst int) *RateLimiter {
	if burst <= 0 {
		burst = perMin
	}
	rl := &RateLimiter{
		buckets: make(map[string]*bucket),
		perMin:  perMin,
		burst:   burst,
		now:     time.Now,
	}
	go rl.cleanupLoop()
	return rl
}

// refill brings b up to date and returns it. Caller holds rl.mu.
func (rl *RateLimiter) refill(key string, now time.Time) *bucket {
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(rl.burst), lastSeen: now}
		rl.buckets[key] = b
		return b
	}
	elapsed := now.Sub(b.lastSeen).Minutes()
	if elapsed > 0 {
		b.tokens += elapsed * float64(rl.perMin)
		if b.tokens > float64(rl.burst) {
			b.tokens = float64(rl.burst)
		}
	}
	b.lastSeen = now
	return b
}

// Allow consumes one token for key and reports whether the request may pass,
// plus the whole tokens left afterwards.
func (rl *RateLimiter) Allow(key string) (bool, int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b := rl.refill(key, rl.now())
	if b.tokens < 1 {
		return false, 0
	}
	b.tokens--
	return true, int(b.tokens)
}

// Len reports the number of tracked buckets (for tests).
func (rl *RateLimiter) Len() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.buckets)
}

// sweep drops buckets idle for longer than bucketIdleTTL.
func (rl *RateLimiter) sweep() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	for k, b := range rl.buckets {
		if now.Sub(b.lastSeen) > bucketIdleTTL {
			delete(rl.buckets, k)
		}
	}
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.sweep()
	}
}

// RateLimitMiddleware applies the per-host limit. Exempt from counting:
// /api/status (health probes) and the MCP GET stream, which is a keep-alive
// channel clients reopen on their own schedule, not a unit of work.
func RateLimitMiddleware(rl *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" || isMCPStream(r) {
			next.ServeHTTP(w, r)
			return
		}

		ok, remaining := rl.Allow(remoteHost(r.RemoteAddr))
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(60.0/float64(rl.perMin)))))
			writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "Too many requests")
			return
		}

		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rl.perMin))
		w.Header().Set("X-RateLimit-Burst", strconv.Itoa(rl.burst))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))

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
