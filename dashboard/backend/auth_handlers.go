package main

import (
	"encoding/json"
	"net/http"
)

const sessionCookieName = "janus_session"

// handleAuthStatus never requires auth itself - the SPA calls this on
// load to decide whether to show the first-run setup screen, the login
// screen, or the real app.
func (a *app) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	authenticated := false
	if c, err := r.Cookie(sessionCookieName); err == nil {
		authenticated = a.auth.ValidSession(c.Value)
	}
	writeJSON(w, http.StatusOK, struct {
		SetupRequired bool `json:"setup_required"`
		Authenticated bool `json:"authenticated"`
	}{SetupRequired: a.auth.SetupRequired(), Authenticated: authenticated})
}

// handleAuthSetup sets the admin password once, on first run only -
// Store.Setup itself refuses a second call, so this can't be used to
// silently reset an existing password.
func (a *app) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.auth.Setup(req.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.startSession(w)
}

func (a *app) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !a.auth.Verify(req.Password) {
		// Deliberately generic - not "wrong password" vs "no such
		// account" (there's only ever one account anyway), no point
		// giving an attacker anything to distinguish.
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	a.startSession(w)
}

func (a *app) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		a.auth.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) startSession(w http.ResponseWriter) {
	token, err := a.auth.NewSession()
	if err != nil {
		http.Error(w, "create session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Secure is safe (not just cosmetic) now that this port is
	// HTTPS-only (see dashboard/backend/main.go's own doc comment) - a
	// browser never sends this cookie in the clear.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", MaxAge: 24 * 60 * 60,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// requireAuth gates a handler behind a valid session cookie - wraps
// every /api/nodes* route, never the /api/auth/* routes themselves or
// static asset serving (the SPA has to load unauthenticated so it can
// render the login/setup screen in the first place).
func (a *app) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil || !a.auth.ValidSession(c.Value) {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
