package auth

import (
	"testing"

	"github.com/charlesrobsampson/aether/internal/config"
)

func TestCheckAccess(t *testing.T) {
	const (
		deniedPhone     = "+15555550999"
		userPhone       = "+15555550102"
		adminPhone      = "+15555550101"
		maintainerPhone = "+15555550103"
		unknownPhone    = "+15555550104"
	)

	cfg := &config.Config{
		Admin: config.AdminConfig{
			Admins:          []string{adminPhone, maintainerPhone},
			ApprovedNumbers: []string{userPhone, adminPhone, maintainerPhone, deniedPhone},
			DeniedNumbers:   []string{deniedPhone},
		},
		Maintainer: config.MaintainerConfig{
			Maintainers: []string{maintainerPhone},
		},
	}

	roles := []struct {
		name  string
		phone string
		want  Role
	}{
		{name: "denied wins over approved", phone: deniedPhone, want: Denied},
		{name: "unknown is denied", phone: unknownPhone, want: Denied},
		{name: "approved is user", phone: userPhone, want: User},
		{name: "admin", phone: adminPhone, want: Admin},
		{name: "maintainer", phone: maintainerPhone, want: Maintainer},
	}

	for _, tc := range roles {
		t.Run("role/"+tc.name, func(t *testing.T) {
			if got := RoleOf(cfg, tc.phone); got != tc.want {
				t.Fatalf("RoleOf(%s) = %v, want %v", tc.phone, got, tc.want)
			}
		})
	}

	// CheckAccess is RoleOf(phone) >= needed for every role transition.
	access := []struct {
		name   string
		phone  string
		needed Role
		want   bool
	}{
		{name: "denied denied", phone: deniedPhone, needed: Denied, want: true},
		{name: "denied user", phone: deniedPhone, needed: User, want: false},
		{name: "denied admin", phone: deniedPhone, needed: Admin, want: false},
		{name: "denied maintainer", phone: deniedPhone, needed: Maintainer, want: false},

		{name: "user denied", phone: userPhone, needed: Denied, want: true},
		{name: "user user", phone: userPhone, needed: User, want: true},
		{name: "user admin", phone: userPhone, needed: Admin, want: false},
		{name: "user maintainer", phone: userPhone, needed: Maintainer, want: false},

		{name: "admin denied", phone: adminPhone, needed: Denied, want: true},
		{name: "admin user", phone: adminPhone, needed: User, want: true},
		{name: "admin admin", phone: adminPhone, needed: Admin, want: true},
		{name: "admin maintainer", phone: adminPhone, needed: Maintainer, want: false},

		{name: "maintainer denied", phone: maintainerPhone, needed: Denied, want: true},
		{name: "maintainer user", phone: maintainerPhone, needed: User, want: true},
		{name: "maintainer admin", phone: maintainerPhone, needed: Admin, want: true},
		{name: "maintainer maintainer", phone: maintainerPhone, needed: Maintainer, want: true},
	}

	for _, tc := range access {
		t.Run("access/"+tc.name, func(t *testing.T) {
			if got := CheckAccess(cfg, tc.phone, tc.needed); got != tc.want {
				t.Fatalf("CheckAccess(%s, %v) = %v, want %v", tc.phone, tc.needed, got, tc.want)
			}
		})
	}
}

// TestChatRoleOf proves group chats cap privileged roles at User while direct
// chats keep the full role.
func TestChatRoleOf(t *testing.T) {
	const (
		userPhone       = "+15555550102"
		adminPhone      = "+15555550101"
		maintainerPhone = "+15555550103"
		groupID         = "group.example"
	)

	cfg := &config.Config{
		Admin: config.AdminConfig{
			Admins:          []string{adminPhone},
			ApprovedNumbers: []string{userPhone, adminPhone, maintainerPhone},
		},
		Maintainer: config.MaintainerConfig{Maintainers: []string{maintainerPhone}},
	}

	tests := []struct {
		name    string
		phone   string
		groupID string
		want    Role
	}{
		{name: "user in group", phone: userPhone, groupID: groupID, want: User},
		{name: "admin in group is capped", phone: adminPhone, groupID: groupID, want: User},
		{name: "maintainer in group is capped", phone: maintainerPhone, groupID: groupID, want: User},
		{name: "admin in private keeps role", phone: adminPhone, want: Admin},
		{name: "maintainer in private keeps role", phone: maintainerPhone, want: Maintainer},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChatRoleOf(cfg, tc.phone, tc.groupID); got != tc.want {
				t.Fatalf("ChatRoleOf(%s, %q) = %v, want %v", tc.phone, tc.groupID, got, tc.want)
			}
		})
	}
}
