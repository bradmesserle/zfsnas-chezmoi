package session

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

const sessionDuration = 24 * time.Hour

// Session holds session data for an authenticated user.
type Session struct {
	Token     string    `json:"token"`
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Store is an in-memory session store.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// Default is the shared global session store.
var Default = &Store{sessions: make(map[string]*Session)}

// Create generates a new session for the given user and stores it.
func (s *Store) Create(userID, username, role string) (*Session, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	now := time.Now()
	sess := &Session{
		Token:     hex.EncodeToString(b),
		UserID:    userID,
		Username:  username,
		Role:      role,
		CreatedAt: now,
		ExpiresAt: now.Add(sessionDuration),
	}
	s.mu.Lock()
	s.sessions[sess.Token] = sess
	s.mu.Unlock()
	return sess, nil
}

// Get retrieves a session by token. Returns nil if not found or expired.
func (s *Store) Get(token string) (*Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[token]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(sess.ExpiresAt) {
		s.Delete(token)
		return nil, false
	}
	return sess, true
}

// Delete removes a session by token.
func (s *Store) Delete(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// List returns all non-expired sessions.
func (s *Store) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if now.Before(sess.ExpiresAt) {
			out = append(out, sess)
		}
	}
	return out
}

// CleanExpired removes all expired sessions.
func (s *Store) CleanExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.sessions {
		if now.After(v.ExpiresAt) {
			delete(s.sessions, k)
		}
	}
}
