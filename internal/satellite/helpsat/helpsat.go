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
	// Trigger is the canonical first token.
	Trigger string
	// Usage is the one-line usage shown in detail view.
	Usage string
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
	cfg := m.configSnapshot()
	role := auth.RoleOf(cfg, sender)
	prefix := helpPrefix(cfg)

	entries := m.entries(cfg, sender, role)
	topic := helpTopic(t)

	if topic == "" {
		return m.send(t, renderList(entries, prefix))
	}
	entry, ok := findEntry(entries, resolveAlias(cfg, sender, topic))
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

// entries returns the visible built-ins plus configured satellites, sorted by
// trigger. Aliases are not separate entries; help resolves them.
func (m *Manager) entries(cfg *config.Config, sender string, role auth.Role) []Entry {
	if m == nil {
		return nil
	}

	out := make([]Entry, 0, len(m.commands)+len(cfg.Satellites))
	seen := make(map[string]bool)
	for _, entry := range m.commands {
		if entry.Trigger == "" || entry.Role > role {
			continue
		}
		seen[entry.Trigger] = true
		out = append(out, entry)
	}

	if cfg != nil {
		names := make([]string, 0, len(cfg.Satellites))
		for name := range cfg.Satellites {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			satellite := cfg.Satellites[name]
			trigger := strings.TrimSpace(satellite.Trigger)
			if trigger == "" || seen[trigger] {
				continue
			}
			seen[trigger] = true
			entry := Entry{
				Name:        satellite.Name,
				Trigger:     trigger,
				Usage:       trigger,
				Description: strings.TrimSpace(satellite.Description),
			}
			if entry.Name == "" {
				entry.Name = name
			}
			if entry.Description == "" {
				entry.Description = "Satellite command"
			}
			for _, command := range satellite.Commands {
				for _, pattern := range command.Patterns {
					entry.Details = append(entry.Details, strings.TrimSpace(pattern))
				}
			}
			out = append(out, entry)
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
		return out[i].Trigger < out[j].Trigger
	})
	return out
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

// renderList is the compact default view: one line per visible command.
func renderList(entries []Entry, prefix string) string {
	if len(entries) == 0 {
		return "No commands are available."
	}
	var b strings.Builder
	b.WriteString("*Available commands*\n")
	for _, entry := range entries {
		fmt.Fprintf(&b, "  *%s%s*", prefix, entry.Usage)
		if entry.Description != "" {
			fmt.Fprintf(&b, " — %s", entry.Description)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Send *%s* for details.", prefix+"help <command>")
	return truncate(b.String())
}

// renderDetail expands one command, including usage, description, and any
// satellite patterns or aliases.
func renderDetail(entry Entry, prefix string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%s%s*\n", prefix, entry.Usage)
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
func helpPrefix(cfg *config.Config) string {
	if cfg == nil {
		return "!"
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
		Recipients: []string{t.Sender},
		Text:       text,
		Format:     "styled",
		Satellite:  satellite,
		ThreadID:   t.ThreadID,
	})
}
