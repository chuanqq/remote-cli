package main

import (
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Session struct {
	ID               string
	WorkingDirectory string
	Environment      []string
	Shell            string
	CreatedAt        time.Time
	ExpiresAt        time.Time
}

type SessionManager struct {
	sessions sync.Map
}

func NewSessionManager() *SessionManager {
	sm := &SessionManager{}
	go sm.cleanupLoop()
	return sm
}

func (sm *SessionManager) Create(req SessionCreateRequest) *Session {
	id := "sess-" + uuid.New().String()[:8]

	dir := req.WorkingDirectory
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}

	shell := req.Shell
	if shell == "" {
		shell = "bash"
	}

	ttl := req.TTLSeconds
	if ttl <= 0 {
		ttl = 3600
	}
	if ttl > 86400 {
		ttl = 86400
	}

	var env []string
	for k, v := range req.Environment {
		env = append(env, k+"="+v)
	}

	sess := &Session{
		ID:               id,
		WorkingDirectory: dir,
		Environment:      env,
		Shell:            shell,
		CreatedAt:        time.Now(),
		ExpiresAt:        time.Now().Add(time.Duration(ttl) * time.Second),
	}

	sm.sessions.Store(id, sess)
	return sess
}

func (sm *SessionManager) Get(id string) *Session {
	v, ok := sm.sessions.Load(id)
	if !ok {
		return nil
	}
	sess := v.(*Session)
	if time.Now().After(sess.ExpiresAt) {
		sm.sessions.Delete(id)
		return nil
	}
	return sess
}

func (sm *SessionManager) Delete(id string) bool {
	_, ok := sm.sessions.LoadAndDelete(id)
	return ok
}

func (sm *SessionManager) Count() int {
	count := 0
	sm.sessions.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

func (sm *SessionManager) UpdateWorkingDirectory(id, dir string) {
	v, ok := sm.sessions.Load(id)
	if !ok {
		return
	}
	sess := v.(*Session)
	sess.WorkingDirectory = dir
}

func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		sm.sessions.Range(func(key, value interface{}) bool {
			sess := value.(*Session)
			if now.After(sess.ExpiresAt) {
				sm.sessions.Delete(key)
			}
			return true
		})
	}
}
