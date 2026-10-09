package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

func AuthMiddleware(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			next.ServeHTTP(w, r)
			return
		}

		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Missing or invalid Authorization header")
			return
		}

		// Constant-time comparison so response timing leaks nothing about how
		// many leading bytes of a guess were right.
		providedToken := []byte(auth[7:])
		if subtle.ConstantTimeCompare(providedToken, want) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid token")
			return
		}

		next.ServeHTTP(w, r)
	})
}
