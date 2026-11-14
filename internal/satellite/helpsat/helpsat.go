// Package helpsat implements Aether's built-in help command.
//
// Help belongs in the router rather than a separate satellite because it needs
// live configuration: the sender's role, the sender's aliases, and the
// satellite commands currently registered. It answers with a short list by
// default and expands one command only when asked.
package helpsat

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

const (
	// SatelliteName identifies the built-in help satellite.
	SatelliteName = "help"
	// maxReplyChars keeps one help reply to a few screenfuls of chat.
	maxReplyChars = 1200
)

// Sender is the outbound seam used to reply through Aether's /message. The
// concrete *sdk.Client satisfies it; tests inject a recorder.
type Sender interface {
	Send(ctx context.Context, resp sdk.Response) error
}

// Entry describes one command shown by help. Built-ins are supplied by the app;
// file-registered satellites are derived from the active configuration.
type Entry struct {
	// Name is the satellite or built-in name used for alias resolution.
	Name string
	// Group is the satellite heading this entry is listed under. Built-ins
	// use "Aether"; satellite entries use the satellite's short prefix when
	// one is configured, otherwise the canonical name.
	Group string
	// Trigger is the canonical first token.
	Trigger string
	// Usage is the one-line usage shown in detail view.
	Usage string
	// Patterns are the satellite command patterns. When present they replace
	// Usage in the list so every command is shown under its satellite.
	Patterns []string
	// Description is the short summary shown in the list.
	Description string
	// Role is the minimum role that may see the command.
	Role auth.Role
	// Details are extra lines shown only for help <trigger>.
	Details []string
}

// Options configures a Manager.
type Options struct {
	// Config returns the active configuration so roles and aliases are live.
	Config func() *config.Config
	// Client replies through Aether. A nil client makes help fail.
	Client Sender
	// Commands are the built-in entries. Satellite entries are appended from
	// Config on each request.
	Commands []Entry
}

// Manager handles the built-in !help command. All fields are immutable after
// New, so it is safe for concurrent use.
type Manager struct {
	config   func() *config.Config
	client   Sender
	commands []Entry
}

// New returns a help handler.
func New(options Options) *Manager {
	return &Manager{
		config:   options.Config,
		client:   options.Client,
		commands: options.Commands,
	}
}

// HandleHelp replies with the command list or one command's details.
func (m *Manager) HandleHelp(t sdk.Transmission) error {
	sender := strings.TrimSpace(t.Sender)
	if sender == "" {
		return sdk.Userf("Help needs a sender.")
	}
	// A group is its own principal: help shows the group's aliases and prefix,
	// and its role is capped at User.
	actor := t.Actor()
	if actor == "" {
		actor = sender
	}
	cfg := m.configSnapshot()
	role := auth.ChatRoleOf(cfg, actor, t.GroupID)
	prefix := helpPrefix(cfg, actor)

	entries := m.entries(cfg, actor, role)
	topic := helpTopic(t)

	if topic == "" {
		return m.send(t, renderList(entries, prefix))
	}
	topic = resolveAlias(cfg, actor, topic)
	if group := groupForTopic(entries, topic); group != "" {
		return m.send(t, renderGroup(entries, group, prefix))
	}
	entry, ok := findEntry(entries, topic)
	if !ok {
		hint := fmt.Sprintf("No command %q. Send %q for the list.", topic, prefix+"help")
		if suggestion := closestEntry(entries, topic); suggestion != "" {
			hint = fmt.Sprintf("No command %q. Did you mean %q? Send %q for the list.",
				topic, suggestion, prefix+"help")
		}
		return m.send(t, hint)
	}
	return m.send(t, renderDetail(entry, prefix))
}

func (m *Manager) configSnapshot() *config.Config {
	if m == nil || m.config == nil {
		return nil
	}
	return m.config()
}

// entries returns the visible built-ins plus configured satellites, grouped by
// satellite and sorted by trigger. Aliases are not separate entries; help
// resolves them.
func (m *Manager) entries(cfg *config.Config, sender string, role auth.Role) []Entry {
	if m == nil {
		return nil
	}

	capacity := len(m.commands)
	if cfg != nil {
		capacity += len(cfg.Satellites)
	}
	out := make([]Entry, 0, capacity)
	for _, entry := range m.commands {
		if entry.Trigger == "" || entry.Role > role {
			continue
		}
		if entry.Group == "" {
			entry.Group = "Aether"
		}
		out = append(out, entry)
	}

	if cfg != nil {
		names := make([]string, 0, len(cfg.Satellites))
		for name := range cfg.Satellites {
			names = append(names, name)
		}
		sort.Strings(names)

		// A satellite's short prefix is its heading. Sibling files that share
		// a canonical name share the same heading, so a satellite split across
		// several files is listed once.
		displayGroups := make(map[string]string)
		for _, name := range names {
			satellite := cfg.Satellites[name]
			canonical := strings.TrimSpace(satellite.Name)
			if canonical == "" {
				canonical = name
			}
			if _, ok := displayGroups[canonical]; !ok {
				displayGroups[canonical] = canonical
			}
			if prefix := strings.TrimSpace(satellite.Prefix); prefix != "" {
				displayGroups[canonical] = prefix
			}
		}

		for _, name := range names {
			satellite := cfg.Satellites[name]
			trigger := strings.TrimSpace(satellite.Trigger)
			if trigger == "" {
				continue
			}
			canonical := strings.TrimSpace(satellite.Name)
			if canonical == "" {
				canonical = name
			}
			entry := Entry{
				Name:        canonical,
				Group:       displayGroups[canonical],
				Trigger:     trigger,
				Usage:       trigger,
				Description: strings.TrimSpace(satellite.Description),
			}
			if entry.Group == "" {
				entry.Group = canonical
			}
			if entry.Description == "" {
				entry.Description = "Satellite command"
			}
			for _, command := range satellite.Commands {
				for _, pattern := range command.Patterns {
					if pattern = strings.TrimSpace(pattern); pattern != "" {
						entry.Patterns = append(entry.Patterns, pattern)
					}
				}
			}
			out = append(out, entry)
		}

		// Satellite settings are edited through the satellite's own config
		// command. It is admin-only and private-chat-only, so it only appears
		// for callers who can use it.
		if role >= auth.Admin {
			type groupInfo struct {
				name  string
				group string
			}
			seen := make(map[string]bool)
			var groups []groupInfo
			for _, name := range names {
				satellite := cfg.Satellites[name]
				canonical := strings.TrimSpace(satellite.Name)
				if canonical == "" {
					canonical = name
				}
				if seen[canonical] {
					continue
				}
				seen[canonical] = true
				groups = append(groups, groupInfo{name: canonical, group: displayGroups[canonical]})
			}
			sort.Slice(groups, func(i, j int) bool { return groups[i].group < groups[j].group })
			for _, group := range groups {
				out = append(out, Entry{
					Name:        group.name,
					Group:       group.group,
					Trigger:     "config",
					Usage:       "config list|get|export|set",
					Patterns:    []string{"config list", "config get <key>", "config export", "config set <key> <value>"},
					Description: "View or change " + group.group + " settings",
					Role:        auth.Admin,
					Details: []string{
						"Satellite config reads and changes require a private chat.",
						"Unprefixed config changes Aether settings instead.",
					},
				})
			}
		}
	}

	// Current aliases belong in the detail view so help shows the shorthand the
	// sender can actually type.
	if cfg != nil {
		for i := range out {
			out[i].Details = append(out[i].Details, aliasesFor(cfg, sender, out[i].Trigger, out[i].Name)...)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := groupRank(out[i].Group), groupRank(out[j].Group)
		if ri != rj {
			return ri < rj
		}
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		if out[i].Trigger != out[j].Trigger {
			return out[i].Trigger < out[j].Trigger
		}
		return out[i].Usage < out[j].Usage
	})
	return out
}

// groupRank puts Aether's built-ins first, then satellites alphabetically.
func groupRank(group string) int {
	if strings.EqualFold(group, "Aether") {
		return 0
	}
	return 1
}

func aliasesFor(cfg *config.Config, sender, trigger, name string) []string {
	user := cfg.Users[sender]
	var aliases []string
	for alias, target := range user.Aliases.Commands {
		if target == trigger {
			aliases = append(aliases, alias)
		}
	}
	for alias, target := range user.Aliases.Satellites {
		if target == name || target == trigger {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	if len(aliases) == 0 {
		return nil
	}
	return []string{"Aliases: " + strings.Join(aliases, ", ")}
}

// resolveAlias maps one of the sender's satellite or command aliases onto its
// canonical trigger/name. Unknown values are returned unchanged.
func resolveAlias(cfg *config.Config, sender, topic string) string {
	if cfg == nil {
		return topic
	}
	user := cfg.Users[sender]
	topic = strings.TrimSpace(topic)
	if target := aliasFold(user.Aliases.Commands, topic); target != "" {
		return target
	}
	if target := aliasFold(user.Aliases.Satellites, topic); target != "" {
		return target
	}
	return topic
}

// aliasFold resolves an alias exactly first, then case-insensitively so a
// capitalized first word from a phone keyboard still works.
func aliasFold(values map[string]string, key string) string {
	if value := values[key]; value != "" {
		return value
	}
	for name, value := range values {
		if strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

// findEntry resolves a canonical trigger, satellite name, or configured alias.
func findEntry(entries []Entry, topic string) (Entry, bool) {
	topic = strings.ToLower(strings.TrimSpace(topic))
	for _, entry := range entries {
		if strings.ToLower(entry.Trigger) == topic || strings.ToLower(entry.Name) == topic {
			return entry, true
		}
	}
	return Entry{}, false
}

// closestEntry suggests an entry whose trigger starts with the same letters.
func closestEntry(entries []Entry, topic string) string {
	topic = strings.ToLower(strings.TrimSpace(topic))
	for _, entry := range entries {
		trigger := strings.ToLower(entry.Trigger)
		if strings.HasPrefix(trigger, topic) || strings.HasPrefix(topic, trigger) {
			return entry.Trigger
		}
	}
	return ""
}

// helpTopic removes the trigger word and returns the remaining argument.
func helpTopic(t sdk.Transmission) string {
	fields := strings.Fields(t.Text)
	if len(fields) == 0 {
		return ""
	}
	switch strings.ToLower(fields[0]) {
	case "help", strings.ToLower(strings.TrimSpace(t.Trigger)):
		fields = fields[1:]
	default:
		return ""
	}
	return strings.TrimSpace(strings.Join(fields, " "))
}

// renderList is the compact default view: every command under its satellite.
func renderList(entries []Entry, prefix string) string {
	if len(entries) == 0 {
		return "No commands are available."
	}
	var b strings.Builder
	b.WriteString("*Available commands*\n")
	for _, group := range groups(entries) {
		fmt.Fprintf(&b, "*%s*\n", group)
		for _, entry := range entries {
			if entry.Group != group {
				continue
			}
			writeEntry(&b, entry, prefix)
		}
	}
	fmt.Fprintf(&b, "Send *%s* for details.", prefix+"help <command>")
	return truncate(b.String())
}

// renderGroup lists every command that belongs to one satellite.
func renderGroup(entries []Entry, group, prefix string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%s*\n", group)
	for _, entry := range entries {
		if entry.Group == group {
			writeEntry(&b, entry, prefix)
		}
	}
	fmt.Fprintf(&b, "Send *%s* for details.", prefix+"help <command>")
	return truncate(b.String())
}

// writeEntry prints one command. Satellite entries print each configured
// pattern so a satellite that owns several commands is fully represented. The
// satellite heading already names the satellite, so the pattern itself is
// shown without a satellite prefix.
func writeEntry(b *strings.Builder, entry Entry, prefix string) {
	if len(entry.Patterns) > 0 {
		for _, pattern := range entry.Patterns {
			fmt.Fprintf(b, "  *%s%s*\n", prefix, pattern)
		}
		return
	}
	fmt.Fprintf(b, "  *%s%s*", prefix, entry.Usage)
	if entry.Description != "" {
		fmt.Fprintf(b, " — %s", entry.Description)
	}
	b.WriteString("\n")
}

// groups returns the display groups in sorted entry order.
func groups(entries []Entry) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Group == "" || seen[entry.Group] {
			continue
		}
		seen[entry.Group] = true
		out = append(out, entry.Group)
	}
	return out
}

// groupForTopic resolves "help <satellite>" to the group heading. The
// satellite's short prefix and canonical name are both accepted.
func groupForTopic(entries []Entry, topic string) string {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return ""
	}
	for _, entry := range entries {
		if entry.Group == "" {
			continue
		}
		if strings.EqualFold(entry.Group, topic) {
			return entry.Group
		}
		// A satellite's canonical name resolves to its heading, which may be
		// the short prefix. Built-in names are command triggers, not groups.
		if !strings.EqualFold(entry.Group, "Aether") &&
			entry.Name != "" && strings.EqualFold(entry.Name, topic) {
			return entry.Group
		}
	}
	return ""
}

// renderDetail expands one command, including usage, description, and any
// satellite patterns or aliases.
func renderDetail(entry Entry, prefix string) string {
	var b strings.Builder
	if len(entry.Patterns) > 0 {
		for _, pattern := range entry.Patterns {
			fmt.Fprintf(&b, "*%s%s*\n", prefix, pattern)
		}
	} else {
		fmt.Fprintf(&b, "*%s%s*\n", prefix, entry.Usage)
	}
	if entry.Description != "" {
		b.WriteString(entry.Description)
		b.WriteString("\n")
	}
	if len(entry.Details) > 0 {
		b.WriteString("\n")
		for _, line := range entry.Details {
			if strings.TrimSpace(line) == "" {
				continue
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(truncate(b.String()))
}

func truncate(text string) string {
	runes := []rune(text)
	if len(runes) <= maxReplyChars {
		return text
	}
	return string(runes[:maxReplyChars-1]) + "…"
}

// helpPrefix returns the first configured command prefix, defaulting to "!".
func helpPrefix(cfg *config.Config, actor string) string {
	if cfg == nil {
		return "!"
	}
	// An explicitly empty user or group prefix list means commands need no
	// prefix and overrides the system persona defaults.
	if user, ok := cfg.Users[actor]; ok && user.Prefixes != nil {
		for _, prefix := range user.Prefixes {
			if prefix = strings.TrimSpace(prefix); prefix != "" {
				return prefix
			}
		}
		return ""
	}
	for _, prefix := range cfg.System.Persona.Prefixes {
		if strings.TrimSpace(prefix) != "" {
			return prefix
		}
	}
	return "!"
}

// send emits one /message response to the originating sender.
func (m *Manager) send(t sdk.Transmission, text string) error {
	if m == nil || m.client == nil {
		return fmt.Errorf("helpsat: outbound client is not configured")
	}
	satellite := strings.TrimSpace(t.Satellite)
	if satellite == "" {
		satellite = SatelliteName
	}
	return m.client.Send(context.Background(), sdk.Response{
		Recipients: []string{t.ReplyTo()},
		Text:       text,
		Format:     "styled",
		Satellite:  satellite,
		ThreadID:   t.ThreadID,
	})
}
