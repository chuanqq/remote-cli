package main

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chuanqq/remote-cli/internal/control"
)

// Cluster token lifecycle (D27/D32). The token is never exposed as an MCP
// tool; only this CLI can create, rotate and retire it.
//
// The master accepts the hash from REMOTECTL_CLUSTER_TOKEN_HASH plus the
// current/next hashes in <state>/master/cluster-tokens.json. Rotation keeps
// one overlap hash so slavers can be reconfigured without a flag day.

const clusterTokensSchemaVersion = 1

type clusterTokensFile struct {
	SchemaVersion int       `json:"schema_version"`
	Current       string    `json:"current"`
	Next          string    `json:"next,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func clusterTokensPath(stateDir string) string {
	return filepath.Join(stateDir, "master", "cluster-tokens.json")
}

func runClusterToken(args []string) {
	if len(args) == 0 {
		clusterTokenUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "create":
		token, err := generateClusterToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: generate token:", err)
			os.Exit(1)
		}
		printClusterToken(token)
	case "rotate":
		fs := flag.NewFlagSet("cluster-token rotate", flag.ExitOnError)
		stateDir := fs.String("state-dir", defaultStateDir(), "deployment state directory")
		oldHash := fs.String("old-hash", "", "hash to keep as the overlap token (default: the current hash in the state file)")
		_ = fs.Parse(args[1:])
		token, hash, err := rotateClusterToken(*stateDir, *oldHash)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println("rotation complete; reconfigure slavers with the new token, then run:")
		fmt.Println("  agentic-remotectl cluster-token finish --state-dir " + *stateDir)
		printClusterToken(token)
		fmt.Printf("\nstate: %s\n  current=%s\n", clusterTokensPath(*stateDir), hash)
	case "finish":
		fs := flag.NewFlagSet("cluster-token finish", flag.ExitOnError)
		stateDir := fs.String("state-dir", defaultStateDir(), "deployment state directory")
		_ = fs.Parse(args[1:])
		if err := finishClusterTokenRotation(*stateDir); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println("overlap token retired; only the current hash is accepted now")
	default:
		clusterTokenUsage()
		os.Exit(1)
	}
}

func clusterTokenUsage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  agentic-remotectl cluster-token create")
	fmt.Fprintln(os.Stderr, "  agentic-remotectl cluster-token rotate [--state-dir DIR] [--old-hash HASH]")
	fmt.Fprintln(os.Stderr, "  agentic-remotectl cluster-token finish [--state-dir DIR]")
}

func printClusterToken(token string) {
	fmt.Println("cluster token (configure on every slaver; it is shown only once):")
	fmt.Printf("  REMOTECTL_CLUSTER_TOKEN=%s\n", token)
	fmt.Println()
	fmt.Println("master configuration (stores only the salted hash):")
	fmt.Printf("  REMOTECTL_CLUSTER_TOKEN_HASH=%s\n", generateTokenHash(token))
}

// rotateClusterToken generates a new token, promotes it to current and moves
// the previous current (or the explicit --old-hash) to next. Any older next
// hash is dropped: only one overlap token is kept.
func rotateClusterToken(stateDir, oldHash string) (token, hash string, err error) {
	token, err = generateClusterToken()
	if err != nil {
		return "", "", err
	}
	hash = generateTokenHash(token)
	path := clusterTokensPath(stateDir)

	var f clusterTokensFile
	_, err = control.ReadJSONFile(path, &f)
	if err != nil && !os.IsNotExist(err) {
		return "", "", fmt.Errorf("read %s: %w", path, err)
	}
	next := oldHash
	if next == "" {
		next = f.Current
	}
	updated := clusterTokensFile{
		SchemaVersion: clusterTokensSchemaVersion,
		Current:       hash,
		Next:          next,
		UpdatedAt:     time.Now(),
	}
	if err := control.WriteJSONAtomic(path, &updated); err != nil {
		return "", "", err
	}
	return token, hash, nil
}

func finishClusterTokenRotation(stateDir string) error {
	path := clusterTokensPath(stateDir)
	var f clusterTokensFile
	found, err := control.ReadJSONFile(path, &f)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no rotation state at %s", path)
	}
	f.Next = ""
	f.UpdatedAt = time.Now()
	return control.WriteJSONAtomic(path, &f)
}

// newClusterTokenVerifier combines the environment hash with the rotation
// state file. The file is read per registration (rare), so rotate/finish take
// effect without restarting the master.
func newClusterTokenVerifier(envHash, statePath string) func(string) bool {
	return func(token string) bool {
		if envHash != "" && verifyTokenHash(envHash, token) {
			return true
		}
		var f clusterTokensFile
		found, err := control.ReadJSONFile(statePath, &f)
		if err != nil || !found || f.SchemaVersion > clusterTokensSchemaVersion {
			return false
		}
		if f.Current != "" && verifyTokenHash(f.Current, token) {
			return true
		}
		return f.Next != "" && verifyTokenHash(f.Next, token)
	}
}

func generateClusterToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "rct_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
