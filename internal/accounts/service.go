package accounts

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
