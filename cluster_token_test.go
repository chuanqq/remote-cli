package main

import (
	"os"
	"testing"

	"github.com/chuanqq/remote-cli/internal/control"
)

func TestClusterTokenRotationLifecycle(t *testing.T) {
	stateDir := t.TempDir()
	path := clusterTokensPath(stateDir)

	envToken := "explicit-env-token"
	verify := newClusterTokenVerifier(generateTokenHash(envToken), path)
	if !verify(envToken) {
		t.Fatal("environment hash token should be accepted")
	}
	if verify("wrong-token") {
		t.Fatal("unknown token must be rejected")
	}

	token1, _, err := rotateClusterToken(stateDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !verify(token1) {
		t.Fatal("first rotated token should be accepted")
	}

	token2, _, err := rotateClusterToken(stateDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !verify(token1) || !verify(token2) {
		t.Fatal("during overlap both the previous and the new token must be accepted")
	}

	token3, _, err := rotateClusterToken(stateDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if verify(token1) {
		t.Fatal("only one overlap token is kept; token1 must be retired")
	}
	if !verify(token2) || !verify(token3) {
		t.Fatal("token2 (overlap) and token3 (current) must be accepted")
	}

	if err := finishClusterTokenRotation(stateDir); err != nil {
		t.Fatal(err)
	}
	if verify(token2) {
		t.Fatal("finish must retire the overlap token")
	}
	if !verify(token3) {
		t.Fatal("current token must remain accepted")
	}
	if !verify(envToken) {
		t.Fatal("the environment hash remains valid after rotation")
	}

	// The state file is readable JSON with the documented schema.
	var f clusterTokensFile
	found, err := control.ReadJSONFile(path, &f)
	if err != nil || !found {
		t.Fatalf("state file unreadable: found=%t err=%v", found, err)
	}
	if f.Current == "" || f.Next != "" {
		t.Fatalf("state file = %+v, want current only", f)
	}
}

func TestMasterConfigAcceptsStateFileToken(t *testing.T) {
	stateDir := t.TempDir()
	cfg := &Config{Mode: modeMaster, ControlListen: "127.0.0.1:1", StateDir: stateDir}
	if err := cfg.validateMasterConfig(); err == nil {
		t.Fatal("master without any cluster token source must be rejected")
	}
	if _, _, err := rotateClusterToken(stateDir, ""); err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateMasterConfig(); err != nil {
		t.Fatalf("state-file token should satisfy master config: %v", err)
	}
	if _, err := os.Stat(clusterTokensPath(stateDir)); err != nil {
		t.Fatal(err)
	}
}
