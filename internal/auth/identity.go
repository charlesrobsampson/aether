package auth

import (
	"regexp"
	"strings"
)

// phonePattern matches an E.164-style phone number. It also keeps an identity
// usable as a file/list key and prevents path traversal through admin commands.
var phonePattern = regexp.MustCompile(`^\+[0-9]{1,20}$`)

// IsPhone reports whether value is an E.164-style phone number.
func IsPhone(value string) bool {
	return phonePattern.MatchString(strings.TrimSpace(value))
}

// IsUUID reports whether value is a canonical 8-4-4-4-12 hexadecimal UUID.
// Signal identifies some senders only by UUID when they do not share a phone
// number, so an approval or a user file must accept one.
func IsUUID(value string) bool {
	parts := strings.Split(strings.TrimSpace(value), "-")
	if len(parts) != 5 {
		return false
	}
	for i, want := range []int{8, 4, 4, 4, 12} {
		if len(parts[i]) != want {
			return false
		}
		for _, r := range parts[i] {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}

// ValidIdentity reports whether value can identify a sender. Both phone
// numbers and Signal UUIDs are accepted because either may arrive as the
// transport's source.
func ValidIdentity(value string) bool {
	return IsPhone(value) || IsUUID(value)
}
