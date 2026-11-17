package helpsat

import (
	"context"
	"fmt"
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

// latest returns the most recent response. Use it when a test deliberately
// sends more than one help request.
func (s *recordingSender) latest(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.responses) == 0 {
		t.Fatal("no responses")
	}
	return s.responses[len(s.responses)-1].Text
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
	t.Run("summary lists satellites without every command", func(t *testing.T) {
		manager, sender := testManager(t, auth.User)
		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help"}); err != nil {
			t.Fatalf("HandleHelp: %v", err)
		}
		got := sender.last(t)
		for _, want := range []string{
			"*Available commands*",
			"*Aether* (3 commands)",
			"*wx* (1 command)",
			"Send *!help <satellite>* for details.",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("help summary does not contain %q:\n%s", want, got)
			}
		}
		for _, unwanted := range []string{"!help [command]", "!fly <station>", "!admin"} {
			if strings.Contains(got, unwanted) {
				t.Errorf("help summary leaked a command %q:\n%s", unwanted, got)
			}
		}
	})

	t.Run("user sees user commands and satellites", func(t *testing.T) {
		manager, sender := testManager(t, auth.User)
		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help aether"}); err != nil {
			t.Fatalf("HandleHelp: %v", err)
		}
		got := sender.latest(t)
		for _, want := range []string{"!help", "!test", "!remind"} {
			if !strings.Contains(got, want) {
				t.Errorf("help list does not contain %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "!admin") {
			t.Errorf("help list leaked an admin command:\n%s", got)
		}

		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help wx"}); err != nil {
			t.Fatalf("HandleHelp: %v", err)
		}
		if got := sender.latest(t); !strings.Contains(got, "*!fly* (1 option)") {
			t.Errorf("satellite help lacks its top-level command:\n%s", got)
		}

		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help wx fly"}); err != nil {
			t.Fatalf("HandleHelp: %v", err)
		}
		if got := sender.latest(t); !strings.Contains(got, "!fly <station>") {
			t.Errorf("command detail lacks its pattern:\n%s", got)
		}
	})

	t.Run("admin sees admin commands", func(t *testing.T) {
		manager, sender := testManager(t, auth.Admin)
		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help aether"}); err != nil {
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

func TestHelpCollapsesLargeGroupToTopLevelCommands(t *testing.T) {
	manager, sender := testManager(t, auth.User)
	cfg := manager.configSnapshot()

	commands := make([]config.CommandConfig, 0, 16)
	for i := 0; i < 16; i++ {
		commands = append(commands, config.CommandConfig{
			Name:        fmt.Sprintf("cmd%d", i),
			Patterns:    []string{fmt.Sprintf("wind command-%02d <value>", i)},
			Description: "A deliberately verbose description that fills the help reply",
		})
	}
	cfg.Satellites["wind"] = config.SatelliteConfig{
		Name:        "wind-predictor",
		Trigger:     "wind",
		Prefix:      "wind",
		Description: "Wind commands",
		Commands:    commands,
	}

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help wind"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.last(t)
	if strings.Contains(got, "…") {
		t.Fatalf("large group was truncated:\n%s", got)
	}
	if !strings.Contains(got, "*!wind* (16 options)") {
		t.Fatalf("group help did not collapse to one command:\n%s", got)
	}
	if strings.Contains(got, "command-15") {
		t.Fatalf("group help listed every option:\n%s", got)
	}

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help wind wind"}); err != nil {
		t.Fatalf("HandleHelp detail: %v", err)
	}
	if detail := sender.latest(t); !strings.Contains(detail, "!wind command-15 <value>") {
		t.Fatalf("command detail hid the last option:\n%s", detail)
	}
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
	if !strings.Contains(resp.Text, "*Aether*") {
		t.Fatalf("styled help lacks a bold group: %q", resp.Text)
	}
}

// TestHelpGroupCapsAdminRole proves an admin in a group sees only User-level
// commands, matching the group privilege cap.
func TestHelpGroupCapsAdminRole(t *testing.T) {
	manager, sender := testManager(t, auth.Admin)
	if err := manager.HandleHelp(sdk.Transmission{
		Sender:  testSender,
		Text:    "help aether",
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
	summary := sender.last(t)
	aether := strings.Index(summary, "*Aether*")
	weather := strings.Index(summary, "*wx*")
	if aether < 0 || weather < 0 {
		t.Fatalf("help summary lacks group headings:\n%s", summary)
	}
	if aether > weather {
		t.Fatalf("Aether group should come first:\n%s", summary)
	}
	if strings.Contains(summary, "!fly <station>") {
		t.Fatalf("help summary listed a command instead of the group:\n%s", summary)
	}

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help wx"}); err != nil {
		t.Fatalf("HandleHelp wx: %v", err)
	}
	got := sender.latest(t)
	if !strings.Contains(got, "*wx*") {
		t.Fatalf("group detail lacks its heading:\n%s", got)
	}
	for _, want := range []string{"*!fly* (1 option)", "*!glide* (1 option)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("group detail lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "wx fly") || strings.Contains(got, "wx glide") {
		t.Fatalf("satellite prefix repeated on every command:\n%s", got)
	}

	for _, command := range []struct {
		topic string
		want  string
	}{
		{"help wx fly", "!fly <station>"},
		{"help wx glide", "!glide <site>"},
	} {
		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: command.topic}); err != nil {
			t.Fatalf("HandleHelp %s: %v", command.topic, err)
		}
		if got := sender.latest(t); !strings.Contains(got, command.want) {
			t.Fatalf("%s lacks %q:\n%s", command.topic, command.want, got)
		}
	}

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help aether"}); err != nil {
		t.Fatalf("HandleHelp aether: %v", err)
	}
	if got := sender.latest(t); !strings.Contains(got, "!help") {
		t.Fatalf("Aether group detail lacks built-ins:\n%s", got)
	}
}

func TestHelpListsCommandDescriptions(t *testing.T) {
	manager, sender := testManager(t, auth.User)
	cfg := manager.configSnapshot()
	cfg.Satellites["glider"] = config.SatelliteConfig{
		Name:        "weather",
		Trigger:     "glider",
		Prefix:      "wx",
		Description: "Glider commands",
		Commands: []config.CommandConfig{
			{Name: "status", Patterns: []string{"glider", "glider status"}, Description: "Check the glider"},
			{Name: "loaded", Patterns: []string{"glider loaded"}, Description: "Mark the glider loaded"},
		},
	}

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help wx"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.last(t)
	if !strings.Contains(got, "*!glider* (3 options)") {
		t.Fatalf("help did not collapse the glider command:\n%s", got)
	}
	if strings.Contains(got, "glider <action>") {
		t.Fatalf("help still shows an opaque action placeholder:\n%s", got)
	}

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help wx glider"}); err != nil {
		t.Fatalf("HandleHelp detail: %v", err)
	}
	detail := sender.latest(t)
	for _, want := range []string{
		"*!glider* — Check the glider",
		"*!glider status* — Check the glider",
		"*!glider loaded* — Mark the glider loaded",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail lacks %q:\n%s", want, detail)
		}
	}

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help glider loaded"}); err != nil {
		t.Fatalf("HandleHelp explicit detail: %v", err)
	}
	explicit := sender.latest(t)
	if !strings.Contains(explicit, "*!glider loaded*") || !strings.Contains(explicit, "Mark the glider loaded") {
		t.Fatalf("detail lacks the explicit command:\n%s", explicit)
	}
}

func TestHelpSatelliteGroupDetail(t *testing.T) {
	manager, sender := testManager(t, auth.User)
	addGlideSatellite(manager)

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help weather"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.last(t)
	for _, want := range []string{"*wx*", "*!fly* (1 option)", "*!glide* (1 option)"} {
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
	for _, want := range []string{"*wx*", "*!fly* (1 option)"} {
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
	if !strings.Contains(got, "*!config* (4 options)") {
		t.Fatalf("group detail lacks the satellite config command:\n%s", got)
	}

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help weather config"}); err != nil {
		t.Fatalf("HandleHelp config: %v", err)
	}
	if detail := sender.latest(t); !strings.Contains(detail, "!config set <key> <value>") {
		t.Fatalf("config detail lacks the set pattern:\n%s", detail)
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

func TestHelpResolvesSatelliteFirstCommand(t *testing.T) {
	manager, sender := testManager(t, auth.User)

	for _, topic := range []string{"help fly", "help wx fly", "help weather fly"} {
		if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: topic}); err != nil {
			t.Fatalf("HandleHelp(%q): %v", topic, err)
		}
		got := sender.latest(t)
		if !strings.Contains(got, "!fly <station>") || !strings.Contains(got, "Aviation weather") {
			t.Errorf("help %q = %q, want the weather fly command", topic, got)
		}
	}
}

func TestHelpAcceptsPrefixOnEveryWord(t *testing.T) {
	manager, sender := testManager(t, auth.User)

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help !wx !fly"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.latest(t)
	if !strings.Contains(got, "!fly <station>") {
		t.Fatalf("per-word prefix help = %q, want the weather fly command", got)
	}
}

func TestHelpAmbiguousCommandNamesPrefixedForms(t *testing.T) {
	manager, sender := testManager(t, auth.User)
	cfg := manager.configSnapshot()
	cfg.Satellites["wind"] = config.SatelliteConfig{
		Name:        "wind-predictor",
		Trigger:     "fly",
		Prefix:      "wind",
		Description: "Wind predictions",
		Commands: []config.CommandConfig{{
			Name:     "fly",
			Patterns: []string{"fly <station>"},
		}},
	}

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help fly"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.latest(t)
	for _, want := range []string{
		`"fly" is available in wind and wx`,
		`"!help !wind !fly"`,
		`"!help !wx !fly"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ambiguous help = %q, want %q", got, want)
		}
	}
}

func TestHelpScopedCommandIsNotAmbiguous(t *testing.T) {
	manager, sender := testManager(t, auth.User)
	cfg := manager.configSnapshot()
	cfg.Satellites["wind"] = config.SatelliteConfig{
		Name:        "wind-predictor",
		Trigger:     "fly",
		Prefix:      "wind",
		Description: "Wind predictions",
		Commands: []config.CommandConfig{{
			Name:     "fly",
			Patterns: []string{"fly <station>"},
		}},
	}

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help wind fly"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	if got := sender.latest(t); !strings.Contains(got, "Wind predictions") {
		t.Fatalf("scoped help = %q, want the wind command", got)
	}
}

func TestHelpNoPrefixUserUsesBareForms(t *testing.T) {
	manager, sender := testManager(t, auth.User)
	cfg := manager.configSnapshot()
	user := cfg.Users[testSender]
	user.Prefixes = []string{}
	cfg.Users[testSender] = user

	if err := manager.HandleHelp(sdk.Transmission{Sender: testSender, Text: "help wx fly"}); err != nil {
		t.Fatalf("HandleHelp: %v", err)
	}
	got := sender.latest(t)
	if !strings.Contains(got, "fly <station>") {
		t.Fatalf("bare help = %q, want the weather fly command", got)
	}
	if strings.Contains(got, "!fly") {
		t.Fatalf("bare help still shows a prefix: %q", got)
	}
}
