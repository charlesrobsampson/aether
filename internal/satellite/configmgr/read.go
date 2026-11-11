package configmgr

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

// command is one parsed !config invocation. action is set, list, get, or
// export; target is the key for set/get; value is only used by set.
type command struct {
	action string
	phone  string
	target string
	value  string
}

// Handle dispatches one !config command. Set commands react through the
// router; list/get/export send their output through Out.
func (m *Manager) Handle(t sdk.Transmission) error {
	cmd, err := parseCommand(t)
	if err != nil {
		return err
	}
	if cmd.action == "set" {
		return m.handleSet(t, setCommand{phone: cmd.phone, target: cmd.target, value: cmd.value})
	}
	return m.handleRead(t, cmd)
}

// SuccessReactionFor makes read-only commands reply with text instead of a
// reaction, while set keeps the 👍 acknowledgement. A bare "config" defaults to
// list, so it must not be acknowledged with a reaction either.
func SuccessReactionFor(text string) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return ""
	}
	switch strings.ToLower(fields[1]) {
	case "list", "get", "export":
		return ""
	}
	return "👍"
}

func parseCommand(t sdk.Transmission) (command, error) {
	if text := strings.TrimSpace(t.Text); text != "" {
		fields := strings.Fields(text)
		if len(fields) > 0 && (strings.EqualFold(fields[0], "config") || (t.Trigger != "" && strings.EqualFold(fields[0], t.Trigger))) {
			fields = fields[1:]
		}
		return parseCommandFields(fields)
	}
	return parseCommandArgs(t.Args)
}

func parseCommandFields(fields []string) (command, error) {
	if len(fields) == 0 {
		return command{action: "list"}, nil
	}

	action := strings.ToLower(fields[0])
	rest := fields[1:]
	switch action {
	case "list", "export":
		phone, err := parsePhoneOnly(action, rest)
		if err != nil {
			return command{}, err
		}
		return command{action: action, phone: phone}, nil
	case "get":
		phone, key, err := parseGetFields(rest)
		if err != nil {
			return command{}, err
		}
		return command{action: "get", phone: phone, target: key}, nil
	case "set":
		set, err := parseSetFields(rest)
		if err != nil {
			return command{}, err
		}
		return command{action: "set", phone: set.phone, target: set.target, value: set.value}, nil
	default:
		return command{}, statusError(http.StatusBadRequest,
			`config: expected "config", "config list", "config get <key>", "config export", or "config set <key> <value>"`)
	}
}

// parsePhoneOnly accepts an optional @<phone> target after list/export.
func parsePhoneOnly(action string, fields []string) (string, error) {
	if len(fields) == 0 {
		return "", nil
	}
	bad := statusError(http.StatusBadRequest, `config: expected "config %s [@phone]"`, action)
	if len(fields) != 1 || !strings.HasPrefix(fields[0], "@") {
		return "", bad
	}
	phone := strings.TrimPrefix(fields[0], "@")
	if phone == "" {
		return "", bad
	}
	return phone, nil
}

// parseGetFields accepts "<key>" or "@<phone> <key>".
func parseGetFields(fields []string) (phone, key string, err error) {
	bad := statusError(http.StatusBadRequest, `config: expected "config get [@phone] <key>"`)
	if len(fields) == 0 {
		return "", "", bad
	}
	if strings.HasPrefix(fields[0], "@") {
		if len(fields) != 2 {
			return "", "", bad
		}
		phone = strings.TrimPrefix(fields[0], "@")
		if phone == "" {
			return "", "", bad
		}
		return phone, fields[1], nil
	}
	if len(fields) != 1 {
		return "", "", bad
	}
	return "", fields[0], nil
}

// parseCommandArgs supports the typed-args form a router matcher may produce.
func parseCommandArgs(args map[string]any) (command, error) {
	action := strings.ToLower(strings.TrimSpace(argString(args, "action")))
	if action == "" {
		if _, ok := argValue(args, "value"); ok {
			action = "set"
		} else {
			action = "list"
		}
	}

	switch action {
	case "list", "export":
		return command{action: action, phone: strings.TrimPrefix(argString(args, "phone"), "@")}, nil
	case "get":
		key := argString(args, "key")
		if key == "" {
			return command{}, statusError(http.StatusBadRequest, `config: expected "config get <key>"`)
		}
		return command{action: "get", phone: strings.TrimPrefix(argString(args, "phone"), "@"), target: key}, nil
	case "set":
		key := argString(args, "key")
		value, ok := argValue(args, "value")
		if key == "" || !ok {
			return command{}, statusBadCommand()
		}
		return command{action: "set", phone: strings.TrimPrefix(argString(args, "phone"), "@"), target: key, value: value}, nil
	default:
		return command{}, statusBadCommand()
	}
}

func (m *Manager) handleRead(t sdk.Transmission, cmd command) error {
	cfg, sender, role, err := m.authorize(t)
	if err != nil {
		return err
	}
	target, err := resolveReadTarget(sender, role, cmd.phone)
	if err != nil {
		return err
	}

	settings := settingsFor(cfg, target)
	var text string
	switch cmd.action {
	case "list":
		text = renderList(settings, cfg, sender, target)
	case "export":
		text = renderExport(settings, cfg, sender, target)
	case "get":
		text, err = renderGet(settings, cfg, sender, target, cmd.target)
		if err != nil {
			return err
		}
	default:
		return statusBadCommand()
	}
	return m.send(t, text)
}

func resolveReadTarget(sender string, role auth.Role, phone string) (string, error) {
	target := strings.TrimSpace(phone)
	if target == "" {
		target = sender
	}
	if target != sender && role < auth.Admin {
		return "", statusError(http.StatusForbidden, "config: admin role required to view another user")
	}
	if !auth.ValidIdentity(target) {
		return "", statusError(http.StatusBadRequest, "config: invalid user %q", target)
	}
	return target, nil
}

// setting is one key/value pair shown by list/get/export. note is non-empty
// when the value is not stored in the user file, for example a default.
type setting struct {
	key   string
	value string
	note  string
}

func settingsFor(cfg *config.Config, phone string) []setting {
	user := cfg.Users[phone]

	out := make([]setting, 0, 6)
	if timezone := strings.TrimSpace(user.Timezone); timezone != "" {
		out = append(out, setting{key: "timezone", value: timezone})
	} else {
		out = append(out, setting{key: "timezone", value: "UTC", note: "default"})
	}

	prefixes, note := effectivePrefixes(cfg, phone)
	value := strings.Join(prefixes, ",")
	if len(prefixes) == 0 {
		value = "(none)"
	}
	out = append(out, setting{key: "prefixes", value: value, note: note})

	if user.Muted {
		out = append(out, setting{key: "muted", value: "true"})
	} else {
		out = append(out, setting{key: "muted", value: "false", note: "default"})
	}

	out = appendAliasSettings(out, "satellites", user.Aliases.Satellites)
	out = appendAliasSettings(out, "commands", user.Aliases.Commands)
	return out
}

func appendAliasSettings(out []setting, group string, aliases map[string]string) []setting {
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, setting{
			key:   "aliases." + group + "." + name,
			value: aliases[name],
		})
	}
	return out
}

// effectivePrefixes returns the prefixes actually in force and, when they do
// not come from the user file, a note describing where they came from.
//
// An explicitly empty user list (JSON []) means "no prefix" and overrides the
// system persona defaults. A missing list still inherits them.
func effectivePrefixes(cfg *config.Config, phone string) ([]string, string) {
	if cfg == nil {
		return []string{"!"}, "default"
	}
	user, hasUser := cfg.Users[phone]
	if hasUser && user.Prefixes != nil {
		return cleanPrefixes(user.Prefixes), ""
	}
	if prefixes := cleanPrefixes(cfg.System.Persona.Prefixes); len(prefixes) > 0 {
		return prefixes, "system default"
	}
	return []string{"!"}, "default"
}

func cleanPrefixes(prefixes []string) []string {
	out := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			out = append(out, prefix)
		}
	}
	return out
}

// actorPrefix returns the first prefix the sender can actually type. An
// explicitly empty user list returns "" because commands need no prefix.
func actorPrefix(cfg *config.Config, sender string) string {
	if cfg != nil {
		user, hasUser := cfg.Users[sender]
		if hasUser && user.Prefixes != nil {
			prefixes := cleanPrefixes(user.Prefixes)
			if len(prefixes) == 0 {
				return ""
			}
			return prefixes[0]
		}
		if prefixes := cleanPrefixes(cfg.System.Persona.Prefixes); len(prefixes) > 0 {
			return prefixes[0]
		}
	}
	return "!"
}

func formatSetting(s setting) string {
	if s.note == "" {
		return s.key + " = " + s.value
	}
	return s.key + " = " + s.value + " (" + s.note + ")"
}

func hasAlias(settings []setting) bool {
	for _, s := range settings {
		if strings.HasPrefix(s.key, "aliases.") {
			return true
		}
	}
	return false
}

// matchSettings returns the exact key or every key below a group such as
// aliases.satellites.
func matchSettings(settings []setting, key string) []setting {
	key = strings.TrimSpace(key)
	var out []setting
	for _, s := range settings {
		if s.key == key || strings.HasPrefix(s.key, key+".") {
			out = append(out, s)
		}
	}
	return out
}

func knownKey(key string) bool {
	switch strings.TrimSpace(key) {
	case "timezone", "prefixes", "muted", "aliases", "aliases.satellites", "aliases.commands":
		return true
	default:
		return false
	}
}

func renderList(settings []setting, cfg *config.Config, actor, target string) string {
	var b strings.Builder
	if target != actor {
		fmt.Fprintf(&b, "Settings for %s:\n", target)
	}
	for _, s := range settings {
		b.WriteString(formatSetting(s))
		b.WriteString("\n")
	}
	if !hasAlias(settings) {
		b.WriteString("aliases = (none)\n")
	}

	example := actorPrefix(cfg, actor) + "config get <key>"
	if target != actor {
		example = actorPrefix(cfg, actor) + "config get @" + target + " <key>"
	}
	fmt.Fprintf(&b, "Send %q for one setting.", example)
	return b.String()
}

func renderGet(settings []setting, cfg *config.Config, actor, target, key string) (string, error) {
	matched := matchSettings(settings, key)
	if len(matched) == 0 {
		if !knownKey(key) {
			return "", statusError(http.StatusBadRequest,
				"config: unknown key %q. Try: timezone, prefixes, muted, aliases.", key)
		}
		return fmt.Sprintf("%s = (none)", strings.TrimSpace(key)), nil
	}

	var b strings.Builder
	for _, s := range matched {
		b.WriteString(formatSetting(s))
		b.WriteString("\n")
	}
	for _, s := range matched {
		b.WriteString("Set with: ")
		b.WriteString(pasteSetCommand(cfg, actor, target, s.key, s.value))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func renderExport(settings []setting, cfg *config.Config, actor, target string) string {
	lines := make([]string, 0, len(settings))
	for _, s := range settings {
		lines = append(lines, pasteSetCommand(cfg, actor, target, s.key, s.value))
	}
	return strings.Join(lines, "\n")
}

// setCommand renders a pasteable !config set line, including a target when an
// admin is looking at somebody else's settings.
func pasteSetCommand(cfg *config.Config, actor, target, key, value string) string {
	if key == "prefixes" && value == "(none)" {
		value = "[]"
	}
	command := actorPrefix(cfg, actor) + "config set"
	if target != actor {
		command += " @" + target
	}
	return command + " " + key + " " + value
}

func (m *Manager) send(t sdk.Transmission, text string) error {
	if m == nil || m.out == nil {
		return statusError(http.StatusServiceUnavailable, "config: outbound client is not configured")
	}
	recipient := strings.TrimSpace(t.Sender)
	if recipient == "" {
		return statusError(http.StatusUnauthorized, "config: sender is required")
	}
	satellite := strings.TrimSpace(t.Satellite)
	if satellite == "" {
		satellite = "config"
	}
	return m.out.Send(context.Background(), sdk.Response{
		Recipients: []string{recipient},
		Text:       text,
		Satellite:  satellite,
		ThreadID:   t.ThreadID,
	})
}
