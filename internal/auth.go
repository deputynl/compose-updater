package internal

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName = "session"
	sessionTTL        = 30 * 24 * time.Hour
	minPasswordLength = 8
)

type authFile struct {
	PasswordHash string `json:"password_hash"`
}

// Auth guards the app behind a single admin password. There is no
// username field on purpose: the only user is "admin".
type Auth struct {
	mu   sync.Mutex
	path string
	data authFile

	sessMu   sync.Mutex
	sessions map[string]time.Time
}

func NewAuth(path string) (*Auth, error) {
	a := &Auth{path: path, sessions: map[string]time.Time{}}
	if err := loadJSON(path, &a.data); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Auth) HasPassword() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.data.PasswordHash != ""
}

// SetPassword hashes and persists a new password, overwriting any
// existing one. Callers are responsible for gating this behind
// !HasPassword() (first run) or an already-authenticated request.
func (a *Auth) SetPassword(password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data.PasswordHash = string(hash)
	return saveJSON(a.path, &a.data)
}

func (a *Auth) CheckPassword(password string) bool {
	a.mu.Lock()
	hash := a.data.PasswordHash
	a.mu.Unlock()
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// NewSession creates a random session token and remembers it in memory.
func (a *Auth) NewSession() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	token := base64.RawURLEncoding.EncodeToString(buf)

	a.sessMu.Lock()
	a.sessions[token] = time.Now().Add(sessionTTL)
	a.sessMu.Unlock()
	return token
}

func (a *Auth) ValidSession(token string) bool {
	if token == "" {
		return false
	}
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	exp, ok := a.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, token)
		return false
	}
	return true
}

func (a *Auth) RevokeSession(token string) {
	a.sessMu.Lock()
	delete(a.sessions, token)
	a.sessMu.Unlock()
}

// RequireAuth wraps a handler, redirecting to /setup or /login as needed.
func (a *Auth) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.HasPassword() {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || !a.ValidSession(cookie.Value) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func SetSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
}

func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
