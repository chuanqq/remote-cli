package main

import (
	"os"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session is a persistent execution context. The working directory (changed
// by a successful `cd`) and the expiry (renewed by every execute) are mutated
// while other calls on the same session read them, so both are guarded by mu
// and must be accessed through Cwd / setCwd / Expiry / touch.
//
// TTL is an idle timeout: each execute pushes the expiry to now + TTL, so a
// session used throughout a long task never expires underneath it (it used to
// expire at a fixed CreatedAt + TTL and silently lose cwd/env mid-task).
type Session struct {
	ID          string
	Environment []string
	Shell       string
	CreatedAt   time.Time
	TTL         time.Duration

	mu               sync.RWMutex
	workingDirectory string
	expiresAt        time.Time
}

// Expiry returns when the session expires unless it is used again.
func (s *Session) Expiry() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.expiresAt
}

// touch renews the session's expiry to now + TTL and returns it.
func (s *Session) touch(now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiresAt = now.Add(s.TTL)
	return s.expiresAt
}

func (s *Session) expired(now time.Time) bool {
	return now.After(s.Expiry())
}

// Cwd returns the session's current working directory.
func (s *Session) Cwd() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workingDirectory
}

func (s *Session) setCwd(dir string) {
	s.mu.Lock()
	s.workingDirectory = dir
	s.mu.Unlock()
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

	now := time.Now()
	sess := &Session{
		ID:               id,
		workingDirectory: dir,
		Environment:      env,
		Shell:            shell,
		CreatedAt:        now,
		TTL:              time.Duration(ttl) * time.Second,
		expiresAt:        now.Add(time.Duration(ttl) * time.Second),
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
	if sess.expired(time.Now()) {
		sm.sessions.Delete(id)
		return nil
	}
	return sess
}

// Touch looks up a live session and renews its expiry (idle TTL). Session
// execute paths use it instead of Get.
func (sm *SessionManager) Touch(id string) *Session {
	sess := sm.Get(id)
	if sess != nil {
		sess.touch(time.Now())
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

// List returns live (non-expired) sessions ordered by creation time.
func (sm *SessionManager) List() []*Session {
	now := time.Now()
	out := make([]*Session, 0)
	sm.sessions.Range(func(_, value interface{}) bool {
		sess := value.(*Session)
		if sess.expired(now) {
			return true // expired: skipped (cleanupLoop reclaims it)
		}
		out = append(out, sess)
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (sm *SessionManager) UpdateWorkingDirectory(id, dir string) {
	v, ok := sm.sessions.Load(id)
	if !ok {
		return
	}
	v.(*Session).setCwd(dir)
}

// sessionResponse is the JSON view of a session.
func sessionResponse(sess *Session) SessionResponse {
	return SessionResponse{
		SessionID:        sess.ID,
		WorkingDirectory: sess.Cwd(),
		CreatedAt:        sess.CreatedAt.Format(time.RFC3339),
		ExpiresAt:        sess.Expiry().Format(time.RFC3339),
		TTLSeconds:       int(sess.TTL / time.Second),
		Shell:            sess.Shell,
	}
}

func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		sm.sessions.Range(func(key, value interface{}) bool {
			sess := value.(*Session)
			if sess.expired(now) {
				sm.sessions.Delete(key)
			}
			return true
		})
	}
}
