package auth

import (
	"strings"

	"github.com/charlesrobsampson/aether/internal/config"
)

// Role is a linear access level. A role grants every capability of the roles
// below it.
type Role int

const (
	// Denied is the zero role for unknown or explicitly denied numbers.
	Denied Role = iota
	// User may edit their own configuration and use user commands.
	User
	// Admin may edit other users and manage satellites and admin.json.
	Admin
	// Maintainer may receive approval requests and manage system notifications.
	Maintainer
)

// RoleOf returns the effective role for phone.
//
// admin.json is the only source of approved and denied numbers: a number in
// denied_numbers is Denied even when it also appears in approved_numbers or
// admins. Maintainers come from maintainer.json. Unknown numbers are Denied.
func RoleOf(cfg *config.Config, phone string) Role {
	if cfg == nil || phone == "" {
		return Denied
	}
	if IsDenied(cfg, phone) {
		return Denied
	}
	if contains(cfg.Maintainer.Maintainers, phone) {
		return Maintainer
	}
	if contains(cfg.Admin.Admins, phone) {
		return Admin
	}
	if contains(cfg.Admin.ApprovedNumbers, phone) {
		return User
	}
	return Denied
}

// ChatRoleOf returns the effective role for a message in a conversation.
//
// Group chats are capped at User: administrative and maintainer actions
// (approvals, editing other users, system notifications) must originate from a
// private chat so they cannot be triggered or observed by other group members.
// Direct chats return the same role as RoleOf.
func ChatRoleOf(cfg *config.Config, phone, groupID string) Role {
	role := RoleOf(cfg, phone)
	if strings.TrimSpace(groupID) != "" && role > User {
		return User
	}
	return role
}

// CheckAccess reports whether phone has at least the needed role.
func CheckAccess(cfg *config.Config, phone string, needed Role) bool {
	return RoleOf(cfg, phone) >= needed
}

// IsDenied reports whether phone is explicitly listed in admin.json's
// denied_numbers. It lets callers distinguish "denied" from "not approved yet"
// without duplicating the access-control lists.
func IsDenied(cfg *config.Config, phone string) bool {
	return cfg != nil && phone != "" && contains(cfg.Admin.DeniedNumbers, phone)
}

// GroupApproved reports whether groupID is listed in admin.json's
// approved_groups. Signal group identifiers are opaque and matched literally.
func GroupApproved(cfg *config.Config, groupID string) bool {
	return cfg != nil && groupID != "" && contains(cfg.Admin.ApprovedGroups, groupID)
}

// GroupDenied reports whether groupID is explicitly listed in admin.json's
// denied_groups.
func GroupDenied(cfg *config.Config, groupID string) bool {
	return cfg != nil && groupID != "" && contains(cfg.Admin.DeniedGroups, groupID)
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
