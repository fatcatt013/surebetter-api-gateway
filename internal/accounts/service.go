package accounts

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Principal is who a request or a WebSocket acts as: a user signed in with a session cookie
// (SessionID set) or with an extension code (CodeID set). It marshals to the /api/auth/me body.
type Principal struct {
	UserID    int64  `json:"-"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Onboarded bool   `json:"onboarded"`
	SessionID int64  `json:"-"`
	CodeID    int64  `json:"-"`
	CodeName  string `json:"code_name,omitempty"`
}

func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }

// Connections is what the service needs from the live WebSocket layer (cmd/gateway's registry).
type Connections interface {
	CloseUser(userID int64)
	CloseSession(sessionID int64)
	CloseCode(codeID int64)
	// NotifyState tells the user's open feed sockets that a state key changed, except the
	// sockets of exceptSession (that tab wrote the value). exceptSession 0 notifies all.
	NotifyState(userID int64, key string, exceptSession int64)
}

type noConnections struct{}

func (noConnections) CloseUser(int64)                  {}
func (noConnections) CloseSession(int64)               {}
func (noConnections) CloseCode(int64)                  {}
func (noConnections) NotifyState(int64, string, int64) {}

// Errors. Their text is the API's "error" value.
var (
	ErrInvalidCredentials = errors.New("invalid_credentials")
	ErrDisabled           = errors.New("disabled")
	ErrEmailTaken         = errors.New("email_taken")
	ErrInvalidEmail       = errors.New("invalid_email")
	ErrWeakPassword       = errors.New("weak_password")
	ErrResetInvalid       = errors.New("reset_invalid")
	ErrNotFound           = errors.New("not_found")
	ErrSelf               = errors.New("cannot_change_self")
	ErrUnknownKey         = errors.New("unknown_key")
	ErrBadValue           = errors.New("invalid_value")
	ErrTooLarge           = errors.New("too_large")
	ErrBadName            = errors.New("invalid_name")
	ErrBadRole            = errors.New("invalid_role")
)

// LockedError: too many failures from this IP or for this email; try again after RetryAfter.
type LockedError struct{ RetryAfter time.Duration }

func (e *LockedError) Error() string { return "locked" }

// InviteError: the invite code cannot be used, and why.
type InviteError struct{ Status InviteStatus }

func (e *InviteError) Error() string { return "invite_invalid: " + string(e.Status) }

// Service is the accounts API over one SQLite database.
type Service struct {
	db         *sql.DB
	now        func() time.Time
	conns      Connections
	emailLimit *limiter // failed logins per email
	ipLimit    *limiter // failed logins, unknown invite and reset codes per client IP
	tickets    *tickets
}

// NewService wraps an opened database (OpenDB). now may be nil (time.Now).
func NewService(db *sql.DB, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		db:         db,
		now:        now,
		conns:      noConnections{},
		emailLimit: newLimiter(5, 15*time.Minute, 15*time.Minute, now),
		ipLimit:    newLimiter(5, 15*time.Minute, 15*time.Minute, now),
		tickets:    newTickets(30*time.Second, now),
	}
}

// SetConnections connects the live WebSocket layer (call once at startup).
func (s *Service) SetConnections(c Connections) {
	if c != nil {
		s.conns = c
	}
}

// IssueTicket returns a 30-second single-use WebSocket ticket for p.
func (s *Service) IssueTicket(p Principal) string { return s.tickets.issue(p) }

// RedeemTicket returns the principal a ticket was issued to; a ticket works once.
func (s *Service) RedeemTicket(t string) (Principal, bool) { return s.tickets.redeem(t) }

func (s *Service) nowMs() int64 { return s.now().UnixMilli() }

func (s *Service) checkIP(ip string) error {
	if ok, wait := s.ipLimit.check(ip); !ok {
		return &LockedError{RetryAfter: wait}
	}
	return nil
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

func nullable(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

// nullID stores 0 as NULL (foreign keys may be absent, e.g. the bootstrap invite's creator).
func nullID(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: id != 0} }

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func validRole(r string) bool { return r == RoleAdmin || r == RoleUser }
