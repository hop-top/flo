package core

import "regexp"

// TenantID names the unit of isolation: its runs, flows, state, approvals
// and grants live apart from every other tenant's. It is used in file names,
// so it is restricted to a safe alphabet.
type TenantID string

// DefaultTenant is the tenant of a single-user install (phase 1).
const DefaultTenant TenantID = "default"

var tenantRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ParseTenant validates s as a tenant id.
func ParseTenant(s string) (TenantID, error) {
	t := TenantID(s)
	if !t.Valid() {
		return "", Errorf(CodeUsage, "tenant %q: want lowercase letters, digits, - and _ (max 63, starting with a letter or digit)", s)
	}
	return t, nil
}

// Valid reports whether t is a well-formed tenant id.
func (t TenantID) Valid() bool { return tenantRe.MatchString(string(t)) }

// String returns the id as text.
func (t TenantID) String() string { return string(t) }

// OrDefault returns t, or DefaultTenant when t is empty. Only entry points
// (the CLI, config) may use it; ports must be handed a tenant.
func (t TenantID) OrDefault() TenantID {
	if t == "" {
		return DefaultTenant
	}
	return t
}

// RequireTenant returns a usage *Error when t is not a valid tenant id.
// Port implementations call it first.
func RequireTenant(t TenantID) error {
	if !t.Valid() {
		return Errorf(CodeUsage, "tenant %q is not a valid tenant id", string(t))
	}
	return nil
}
