package accounts

import (
	"encoding/json"
	"net/http"
	"strings"
)

const smallBody = 16 << 10

// Handler serves /api/auth/*, /api/me/* and /api/admin/*, behind CSRF. Mount it on those three
// prefixes.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	open := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, h) }
	auth := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.RequireAuth(h)) }
	session := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.RequireSession(h)) }
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.RequireAdmin(h)) }

	open("POST /api/auth/invite/check", s.handleInviteCheck)
	open("POST /api/auth/register", s.handleRegister)
	open("POST /api/auth/login", s.handleLogin)
	open("POST /api/auth/reset/check", s.handleResetCheck)
	open("POST /api/auth/reset", s.handleReset)
	session("POST /api/auth/logout", s.handleLogout)
	auth("GET /api/auth/me", s.handleMe)
	auth("POST /api/auth/ws-ticket", s.handleTicket)

	session("POST /api/me/onboarded", s.handleOnboarded)
	auth("GET /api/me/state", s.handleGetState)
	session("PUT /api/me/state/{key}", s.handlePutState)
	session("GET /api/me/extension-codes", s.handleListCodes)
	session("POST /api/me/extension-codes", s.handleCreateCode)
	session("DELETE /api/me/extension-codes/{id}", s.handleRevokeCode)

	admin("GET /api/admin/invites", s.handleListInvites)
	admin("POST /api/admin/invites", s.handleCreateInvite)
	admin("DELETE /api/admin/invites/{id}", s.handleRevokeInvite)
	admin("GET /api/admin/users", s.handleListUsers)
	admin("POST /api/admin/users/{id}/disable", s.handleSetDisabled(true))
	admin("POST /api/admin/users/{id}/enable", s.handleSetDisabled(false))
	admin("POST /api/admin/users/{id}/reset", s.handleCreateReset)
	return CSRF(mux)
}

type credentials struct {
	Code     string `json:"code"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Service) handleInviteCheck(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !decodeJSON(w, r, &in, smallBody) {
		return
	}
	st, err := s.CheckInvite(clientIP(r), in.Code)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := map[string]any{"valid": st == InviteValid}
	if st != InviteValid {
		out["reason"] = st
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !decodeJSON(w, r, &in, smallBody) {
		return
	}
	p, token, err := s.Register(clientIP(r), in.Code, in.Email, in.Password)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, p)
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !decodeJSON(w, r, &in, smallBody) {
		return
	}
	p, token, err := s.Login(clientIP(r), in.Email, in.Password)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, p)
}

func (s *Service) handleResetCheck(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !decodeJSON(w, r, &in, smallBody) {
		return
	}
	email, ok, err := s.CheckReset(clientIP(r), in.Code)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := map[string]any{"valid": ok}
	if ok {
		out["email"] = email
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleReset(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !decodeJSON(w, r, &in, smallBody) {
		return
	}
	p, token, err := s.ResetPassword(clientIP(r), in.Code, in.Password)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, p)
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.Logout(principal(r).SessionID); err != nil {
		writeServiceError(w, err)
		return
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, principal(r))
}

func (s *Service) handleTicket(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"ticket": s.IssueTicket(principal(r))})
}

func (s *Service) handleOnboarded(w http.ResponseWriter, r *http.Request) {
	if err := s.SetOnboarded(principal(r).UserID); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleGetState(w http.ResponseWriter, r *http.Request) {
	m, err := s.GetState(principal(r).UserID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Service) handlePutState(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Value json.RawMessage `json:"value"`
	}
	if !decodeJSON(w, r, &in, MaxStateBytes+4096) {
		return
	}
	if err := s.PutState(principal(r), r.PathValue("key"), in.Value, clientID(r)); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleListCodes(w http.ResponseWriter, r *http.Request) {
	list, err := s.ListExtensionCodes(principal(r).UserID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Service) handleCreateCode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &in, smallBody) {
		return
	}
	c, err := s.CreateExtensionCode(principal(r).UserID, in.Name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Service) handleRevokeCode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.RevokeExtensionCode(principal(r).UserID, id); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleListInvites(w http.ResponseWriter, r *http.Request) {
	list, err := s.ListInvites()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Service) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &in, smallBody) {
		return
	}
	inv, err := s.CreateInvite(principal(r).UserID, in.Role)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, inv)
}

func (s *Service) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.RevokeInvite(id); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleListUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.ListUsers()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Service) handleSetDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if err := s.SetDisabled(principal(r).UserID, id, disabled); err != nil {
			writeServiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Service) handleCreateReset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	reset, err := s.CreateReset(principal(r).UserID, id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, reset)
}

// clientID reads the optional X-Client-Id header; invalid values are ignored.
func clientID(r *http.Request) string {
	return ValidClientID(strings.TrimSpace(r.Header.Get("X-Client-Id")))
}

// ValidClientID returns id if it is at most 64 chars of [A-Za-z0-9_-], else "".
func ValidClientID(id string) string {
	if len(id) > 64 {
		return ""
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return ""
		}
	}
	return id
}
