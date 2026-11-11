package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// CookieName is the session cookie. Its Max-Age is long; the server-side expiry (30 days,
// extended on use) is what ends a session.
const CookieName = "sb_session"

const cookieMaxAge = 400 * 24 * time.Hour

type principalKey struct{}

// PrincipalFrom returns the principal RequireAuth stored on the request context.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

func principal(r *http.Request) Principal {
	p, _ := PrincipalFrom(r.Context())
	return p
}

// Authenticate resolves a request's credential. When an Authorization header is present only
// the Bearer extension code counts and the cookie is ignored, so CSRF may trust such requests.
func (s *Service) Authenticate(r *http.Request) (Principal, bool, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		code, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			return Principal{}, false, nil
		}
		return s.AuthenticateCode(code)
	}
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return Principal{}, false, nil
	}
	return s.AuthenticateSession(c.Value)
}

// RequireAuth admits a session cookie or an extension code.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok, err := s.Authenticate(r)
		if err != nil {
			internalError(w, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// RequireSession admits only a signed-in browser session (extension codes are read-only).
func (s *Service) RequireSession(next http.Handler) http.Handler {
	return s.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal(r).SessionID == 0 {
			writeErr(w, http.StatusForbidden, "session_required")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// RequireAdmin admits only an admin's browser session.
func (s *Service) RequireAdmin(next http.Handler) http.Handler {
	return s.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !principal(r).IsAdmin() {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// SameHost reports whether origin (an Origin header value) names the request's host. Ports are
// ignored: nginx forwards Host without the port, and another port on the same host is the same site.
func SameHost(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.EqualFold(u.Hostname(), host)
}

// CSRF rejects state-changing browser requests from other sites: they need an Origin naming
// this host and, for POST/PUT, a JSON content type (which a plain HTML form cannot send).
// Requests with a Bearer Authorization header are exempt: Authenticate then ignores the cookie
// (a browser cannot attach one cross-site without a CORS preflight, which gets no CORS answer).
func CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			if !SameHost(r.Header.Get("Origin"), r) {
				writeErr(w, http.StatusForbidden, "bad_origin")
				return
			}
			if (r.Method == http.MethodPost || r.Method == http.MethodPut) &&
				!isJSON(r.Header.Get("Content-Type")) {
				writeErr(w, http.StatusUnsupportedMediaType, "json_required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(cookieMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.Header.Get("X-Forwarded-Proto") == "https",
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.Header.Get("X-Forwarded-Proto") == "https"})
}

// clientIP: nginx's X-Real-IP, else the TCP peer.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// decodeJSON reads a JSON body of at most max bytes; an empty body leaves v unchanged.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, max int64) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, max)).Decode(v)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large")
		return false
	}
	writeErr(w, http.StatusBadRequest, "invalid_json")
	return false
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusNotFound, "not_found")
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func internalError(w http.ResponseWriter, err error) {
	log.Error().Err(err).Msg("accounts: internal error")
	writeErr(w, http.StatusInternalServerError, "internal")
}

// writeServiceError maps service errors to HTTP answers ({"error": <code>} bodies).
func writeServiceError(w http.ResponseWriter, err error) {
	var locked *LockedError
	var inv *InviteError
	switch {
	case errors.As(err, &locked):
		secs := int(math.Ceil(locked.RetryAfter.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "locked", "retry_after": secs})
	case errors.As(err, &inv):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invite_invalid", "reason": inv.Status})
	case errors.Is(err, ErrEmailTaken):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrInvalidCredentials):
		writeErr(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, ErrDisabled):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, ErrInvalidEmail), errors.Is(err, ErrWeakPassword), errors.Is(err, ErrResetInvalid),
		errors.Is(err, ErrUnknownKey), errors.Is(err, ErrBadValue), errors.Is(err, ErrBadName),
		errors.Is(err, ErrBadRole), errors.Is(err, ErrSelf):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		internalError(w, err)
	}
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/json"
}
