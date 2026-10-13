package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// tokenAuth verifies a Bearer credential. Two storage modes, chosen by which
// env var the operator set (mutually exclusive, enforced at startup):
//
//   - plain (SHELL_API_TOKEN): the configured value IS the token. Legacy
//     behavior, unchanged.
//   - hashed (SHELL_API_TOKEN_HASH): the configured value is
//     "sha256:<salt-hex>:<hash-hex>" where hash = SHA-256(salt || token). The
//     server never holds the plaintext, so a leaked .env / env.conf /
//     /proc/<pid>/environ is not a usable credential.
//
// The wire protocol is identical in both modes: the client still sends
// "Authorization: Bearer <plaintext-token>". Only the server-side storage
// differs, which is what makes the mode switch invisible to existing clients.
type tokenAuth struct {
	verify func(provided string) bool
}

func newPlainAuth(token string) *tokenAuth {
	want := []byte(token)
	return &tokenAuth{verify: func(provided string) bool {
		return subtle.ConstantTimeCompare([]byte(provided), want) == 1
	}}
}

func newHashAuth(salt, wantHash []byte) *tokenAuth {
	return &tokenAuth{verify: func(provided string) bool {
		got := hashToken(salt, provided)
		return subtle.ConstantTimeCompare(got, wantHash) == 1
	}}
}

// hashToken is THE definition of the stored digest: SHA-256 over the salt
// bytes followed by the UTF-8 bytes of the token.
func hashToken(salt []byte, token string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(token))
	return h.Sum(nil)
}

const (
	// hashTokenSaltBytes is the generated salt size: enough to make every
	// deployment's digest unique, defeating precomputed (rainbow-table)
	// attacks even against low-entropy passwords.
	hashTokenSaltBytes = 16
	// hashTokenMinSaltBytes is the accepted minimum at parse time.
	hashTokenMinSaltBytes = 8
)

// parseTokenHash decodes a SHELL_API_TOKEN_HASH value. The leading algorithm
// tag keeps the format extensible (a slow KDF can slot in later as a new tag)
// while staying grep-able in a config file.
//
// The separator is ':' — not the '$' of crypt/bcrypt style — on purpose:
// .env and env.conf files are sourced by shell scripts, and a '$' in the
// value would be variable-expanded (or trip `set -u`) at load time.
func parseTokenHash(s string) (salt, wantHash []byte, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 || parts[0] != "sha256" {
		return nil, nil, errors.New("expected format sha256:<salt-hex>:<hash-hex>")
	}
	if salt, err = hex.DecodeString(parts[1]); err != nil {
		return nil, nil, fmt.Errorf("salt is not valid hex: %w", err)
	}
	if len(salt) < hashTokenMinSaltBytes {
		return nil, nil, fmt.Errorf("salt must be at least %d bytes, got %d", hashTokenMinSaltBytes, len(salt))
	}
	if wantHash, err = hex.DecodeString(parts[2]); err != nil {
		return nil, nil, fmt.Errorf("hash is not valid hex: %w", err)
	}
	if len(wantHash) != sha256.Size {
		return nil, nil, fmt.Errorf("hash must be %d bytes, got %d", sha256.Size, len(wantHash))
	}
	return salt, wantHash, nil
}

// generateTokenHash salts and digests token and returns the value to put in
// SHELL_API_TOKEN_HASH.
func generateTokenHash(token string) string {
	salt := make([]byte, hashTokenSaltBytes)
	_, _ = rand.Read(salt) // crypto/rand never returns an error in practice; it panics on failure
	return "sha256:" + hex.EncodeToString(salt) + ":" + hex.EncodeToString(hashToken(salt, token))
}

// buildTokenAuth turns the two token env vars into one verifier, enforcing
// exactly-one-of semantics. It also returns the mode name for the startup log.
func buildTokenAuth(token, tokenHash string) (*tokenAuth, string, error) {
	switch {
	case token != "" && tokenHash != "":
		return nil, "", errors.New("SHELL_API_TOKEN and SHELL_API_TOKEN_HASH are mutually exclusive; set exactly one")
	case token != "":
		return newPlainAuth(token), "token", nil
	case tokenHash != "":
		salt, want, err := parseTokenHash(tokenHash)
		if err != nil {
			return nil, "", fmt.Errorf("SHELL_API_TOKEN_HASH: %w", err)
		}
		return newHashAuth(salt, want), "token-hash", nil
	default:
		return nil, "", errors.New("SHELL_API_TOKEN (plaintext) or SHELL_API_TOKEN_HASH (salted SHA-256) is required")
	}
}

func AuthMiddleware(auth *tokenAuth, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			next.ServeHTTP(w, r)
			return
		}

		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Missing or invalid Authorization header")
			return
		}

		// Verification is constant-time in both modes so response timing
		// leaks nothing about how many leading bytes of a guess were right.
		if !auth.verify(authz[len("Bearer "):]) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid token")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// runHashToken implements the `hash-token` subcommand: read the token from
// stdin (one line) and print a SHELL_API_TOKEN_HASH value on stdout. Reading
// stdin rather than argv keeps the plaintext out of shell history and ps.
func runHashToken() {
	if stat, err := os.Stdin.Stat(); err == nil && (stat.Mode()&os.ModeCharDevice) != 0 {
		fmt.Fprint(os.Stderr, "token (input is echoed locally, then hashed): ")
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "error: read stdin:", err)
		os.Exit(1)
	}
	token := strings.TrimRight(line, "\r\n")
	if token == "" {
		fmt.Fprintln(os.Stderr, "error: empty token")
		os.Exit(1)
	}
	fmt.Println(generateTokenHash(token))
}
