package helpsat

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/aether/internal/auth"
	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

const testSender = "+15555550102"

type recordingSender struct {
	mu        sync.Mutex
	responses []sdk.Response
}

func (s *recordingSender) Send(_ context.Context, resp sdk.Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, resp)
	return nil
}

func (s *recordingSender) last(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(s.responses))
	}
	return s.responses[0].Text
}

func testManager(t *testing.T, role auth.Role) (*Manager, *recordingSender) {
	t.Helper()

	cfg := &config.Config{}
	cfg.System.Persona.Prefixes = []string{"!"}
	// A group is its own principal; this fixture marks group.example approved
	// so group tests get User-level access.
	cfg.Admin.ApprovedGroups = []string{"group.example"}
	cfg.Satellites = map[string]config.SatelliteConfig{
		"weather": {
			Name:        "weather",
			Trigger:     "fly",
			Prefix:      "wx",
			Description: "Aviation weather",
			Commands: []config.CommandConfig{{
				Name:     "fly",
				Patterns: []string{"fly <station>"},
			}},
		},
	}
	switch role {
	case auth.User:
		cfg.Admin.ApprovedNumbers = []string{testSender}
	case auth.Admin:
		cfg.Admin.Admins = []string{testSender}
	}
	cfg.Users = map[string]config.UserConfig{
		testSender: {Aliases: config.Aliases{Commands: map[string]string{"r": "remind"}}},
	}

	sender := &recordingSender{}
	manager := New(Options{
		Config: func() *config.Config { return cfg },
		Client: sender,
		Commands: []Entry{
			{Name: "help", Trigger: "help", Usage: "help [command]", Description: "List available commands", Role: auth.User},
			{Name: "test", Trigger: "test", Usage: "test", Description: "Check the connection", Role: auth.User},
			{Name: "remind", Trigger: "remind", Usage: "remind me <text> in <count> <unit>", Description: "Set a reminder", Role: auth.User},
			{Name: "admin", Trigger: "admin", Usage: "admin <approve|deny|list> ...", Description: "Manage users", Role: auth.Admin},
		},
	})
	return manager, sender
}

func TestHelpListIsRoleAware(t *testing.T) {
	t.Run("user sees user commands and satellites", func(t *testing.T) {
		manager, sender := testManager(t, auth.User)
		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help"}); err != nil {
			t.Fatalf("HandleHelp: %v", err)
		}
		got := sender.last(t)
		for _, want := range []string{"!help", "!test", "!remind", "!fly <station>"} {
			if !strings.Contains(got, want) {
				t.Errorf("help list does not contain %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "!admin") {
			t.Errorf("help list leaked an admin command:\n%s", got)
		}
	})

	t.Run("admin sees admin commands", func(t *testing.T) {
		manager, sender := testManager(t, auth.Admin)
		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help"}); err != nil {
			t.Fatalf("HandleHelp: %v", err)
		}
		if got := sender.last(t); !strings.Contains(got, "!admin") {
			t.Errorf("help list lacks an admin command:\n%s", got)
		}
	})
}

func TestHelpDetail(t *testing.T) {
	t.Run("satellite details include patterns", func(t *testing.T) {
		manager, sender := testManager(t, auth.User)
		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help fly"}); err != nil {
			t.Fatalf("HandleHelp: %v", err)
		}
		got := sender.last(t)
		if !strings.Contains(got, "Aviation weather") || !strings.Contains(got, "fly <station>") {
			t.Fatalf("detail = %q", got)
		}
	})

	t.Run("alias resolves to the canonical command", func(t *testing.T) {
		manager, sender := testManager(t, auth.User)
		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help r"}); err != nil {
			t.Fatalf("HandleHelp: %v", err)
		}
		got := sender.last(t)
		if !strings.Contains(got, "!remind me <text> in <count> <unit>") {
			t.Fatalf("detail = %q", got)
		}
		if !strings.Contains(got, "Aliases: r") {
			t.Fatalf("detail lacks the alias: %q", got)
		}
	})

	t.Run("unknown topic suggests a close match", func(t *testing.T) {
		manager, sender := testManager(t, auth.User)
		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help rem"}); err != nil {
			t.Fatalf("HandleHelp: %v", err)
		}
		got := sender.last(t)
		if !strings.Contains(got, "Did you mean") || !strings.Contains(got, "remind") {
			t.Fatalf("reply = %q", got)
		}
	})
}

func TestHelpCapsOutput(t *testing.T) {
	manager, sender := testManager(t, auth.User)
	manager.commands = append(manager.commands, Entry{
		Name: "verbose", Trigger: "verbose", Usage: "verbose", Role: auth.User,
		Details: []string{strings.Repeat("x", 4000)},
	})
	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help verbose"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	if got := []rune(sender.last(t)); len(got) > maxReplyChars {
		t.Fatalf("help reply length = %d, want <= %d", len(got), maxReplyChars)
	}
}

func TestHelpRepliesToGroup(t *testing.T) {
	manager, sender := testManager(t, auth.User)
	if err := manager.HandleHelp(sdk.Transmission{
		Sender:  testSender,
		GroupID: "group.example",
		Text:    "help",
	}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	if len(sender.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(sender.responses))
	}
	got := sender.responses[0].Recipients
	if len(got) != 1 || got[0] != "group.example" {
		t.Fatalf("recipients = %v, want [group.example]", got)
	}
}

func TestHelpSendsStyledText(t *testing.T) {
	manager, sender := testManager(t, auth.User)
	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	if len(sender.responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(sender.responses))
	}
	resp := sender.responses[0]
	if resp.Format != "styled" {
		t.Fatalf("format = %q, want styled", resp.Format)
	}
	if !strings.Contains(resp.Text, "*Available commands*") {
		t.Fatalf("styled help lacks a bold header: %q", resp.Text)
	}
	if !strings.Contains(resp.Text, "*!help") {
		t.Fatalf("styled help lacks a bold command: %q", resp.Text)
	}
}

// TestHelpGroupCapsAdminRole proves an admin in a group sees only User-level
// commands, matching the group privilege cap.
func TestHelpGroupCapsAdminRole(t *testing.T) {
	manager, sender := testManager(t, auth.Admin)
	if err := manager.HandleHelp(sdk.Transmission{
		Sender:  testSender,
		Text:    "help",
		GroupID: "group.example",
	}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.last(t)
	if strings.Contains(got, "!admin") {
		t.Fatalf("group help leaked an admin command:\n%s", got)
	}
	if !strings.Contains(got, "!test") {
		t.Fatalf("group help lacks user commands:\n%s", got)
	}
}

func addGlideSatellite(manager *Manager) {
	cfg := manager.configSnapshot()
	cfg.Satellites["glider"] = config.SatelliteConfig{
		Name:        "weather",
		Trigger:     "glide",
		Prefix:      "wx",
		Description: "Glider status",
		Commands: []config.CommandConfig{{
			Name:     "glide",
			Patterns: []string{"glide <site>"},
		}},
	}
}

func TestHelpGroupsCommandsBySatellite(t *testing.T) {
	manager, sender := testManager(t, auth.User)
	addGlideSatellite(manager)

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.last(t)

	aether := strings.Index(got, "*Aether*")
	weather := strings.Index(got, "*wx*")
	if aether < 0 || weather < 0 {
		t.Fatalf("help lacks group headings:\n%s", got)
	}
	if aether > weather {
		t.Fatalf("Aether group should come first:\n%s", got)
	}
	if !strings.Contains(got[:weather], "!help") {
		t.Fatalf("built-in help is not under Aether:\n%s", got)
	}
	underWeather := got[weather:]
	if !strings.Contains(underWeather, "!fly <station>") {
		t.Fatalf("fly is not under weather:\n%s", got)
	}
	if !strings.Contains(underWeather, "!glide <site>") {
		t.Fatalf("glide is not under weather:\n%s", got)
	}
	if strings.Contains(underWeather, "wx fly") || strings.Contains(underWeather, "wx glide") {
		t.Fatalf("satellite prefix repeated on every command:\n%s", got)
	}
}

func TestHelpSatelliteGroupDetail(t *testing.T) {
	manager, sender := testManager(t, auth.User)
	addGlideSatellite(manager)

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help weather"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.last(t)
	for _, want := range []string{"*wx*", "!fly <station>", "!glide <site>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("group detail lacks %q:\n%s", want, got)
		}
	}
}

func TestHelpSatelliteGroupShortPrefix(t *testing.T) {
	manager, sender := testManager(t, auth.User)

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help wx"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.last(t)
	for _, want := range []string{"*wx*", "!fly <station>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("short-prefix group detail lacks %q:\n%s", want, got)
		}
	}
}

func TestHelpListsSatelliteConfigForAdmins(t *testing.T) {
	manager, sender := testManager(t, auth.Admin)

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help weather"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.last(t)
	if !strings.Contains(got, "!config set <key> <value>") {
		t.Fatalf("group detail lacks the satellite config command:\n%s", got)
	}
}

func TestHelpHidesSatelliteConfigFromUsers(t *testing.T) {
	manager, sender := testManager(t, auth.User)

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help weather"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.last(t)
	if strings.Contains(got, "config set <key> <value>") {
		t.Fatalf("user help leaked the admin satellite config command:\n%s", got)
	}
}
