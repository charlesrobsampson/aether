package config

import (
	"encoding/base64"
	"path/filepath"
	"strings"
)

// groupIDPrefix distinguishes a group conversation from a phone number or
// Signal UUID. Group IDs are opaque and matched literally.
const groupIDPrefix = "group."

// IsGroupID reports whether identity names a group conversation.
func IsGroupID(identity string) bool {
	return strings.HasPrefix(strings.TrimSpace(identity), groupIDPrefix)
}

// GroupFileKey encodes a group ID as a filesystem-safe file name stem. Signal
// group IDs contain '/', '+', and '=', so the raw ID cannot be a file name.
func GroupFileKey(groupID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(groupID))
}

// DecodeGroupFileKey reverses GroupFileKey. Both padded and unpadded URL-safe
// encodings are accepted so a hand-written file still loads.
func DecodeGroupFileKey(key string) (string, bool) {
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding} {
		raw, err := encoding.DecodeString(key)
		if err != nil {
			continue
		}
		identity := string(raw)
		if IsGroupID(identity) {
			return identity, true
		}
	}
	return "", false
}

// UserFilePath returns the settings file for identity. Direct chats live under
// users/ and groups under groups/, but both decode into the same Users map.
func UserFilePath(root, identity string) string {
	identity = strings.TrimSpace(identity)
	if IsGroupID(identity) {
		return filepath.Join(root, "groups", GroupFileKey(identity)+".json")
	}
	return filepath.Join(root, "users", identity+".json")
}
