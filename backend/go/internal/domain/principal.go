package domain

import (
	"time"

	"github.com/google/uuid"
)

// LocalTenantID is the single-tenant local build tenant.
var LocalTenantID = uuid.MustParse("11111111-1111-4111-8111-111111111111")

// LocalUserID is the local build user.
const LocalUserID = "local-dev"

// Principal is the immutable request subject.
type Principal struct {
	TenantID uuid.UUID
	UserID   string
	Roles    []string
	Groups   []string
}

// LocalPrincipal returns the single-tenant admin principal.
func LocalPrincipal() Principal {
	return Principal{
		TenantID: LocalTenantID,
		UserID:   LocalUserID,
		Roles:    []string{"admin"},
		Groups:   []string{},
	}
}

// HasRole reports membership in role.
func (p Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// ACL holds access rules as stored, without normalization.
type ACL struct {
	Roles  []string `json:"roles"`
	Groups []string `json:"groups"`
	Users  []string `json:"users"`
}

// AllowsAny reports empty ACL, which means access for every tenant principal.
func (a ACL) AllowsAny() bool {
	return len(a.Roles) == 0 && len(a.Groups) == 0 && len(a.Users) == 0
}

// Grants reports whether p may access.
func (a ACL) Grants(p Principal) bool {
	if a.AllowsAny() {
		return true
	}
	for _, role := range a.Roles {
		if p.HasRole(role) {
			return true
		}
	}
	for _, user := range a.Users {
		if user == p.UserID {
			return true
		}
	}
	for _, group := range a.Groups {
		for _, principalGroup := range p.Groups {
			if principalGroup == group {
				return true
			}
		}
	}
	return false
}

// KB is a knowledge base; settings come from SettingsRepository.
type KB struct {
	KBID               string
	Name               string
	EmbeddingProfileID string
	CreatedAt          time.Time
}

// CreateKBInput is the KB creation input.
type CreateKBInput struct {
	Name string
	ACL  ACL
}

// AllowedList is per-KB/document access; empty means open to the tenant.
type AllowedList struct {
	Users  []string `json:"users"`
	Groups []string `json:"groups"`
}

// AllowsAny reports empty list, which means open to the tenant.
func (a AllowedList) AllowsAny() bool { return len(a.Users) == 0 && len(a.Groups) == 0 }
