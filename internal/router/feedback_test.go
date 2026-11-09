package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

// feedbackRouter builds a router with one user and recording reply/reaction
// callbacks. No store or HTTP client is needed because every command is a
// built-in.
func feedbackRouter(reply func(string), react func(string) error) *Router {
	cfg := &config.Config{}
	cfg.System.Persona.Prefixes = []string{"!"}
	cfg.Users = map[string]config.UserConfig{
		"+15555550102": {Timezone: "UTC"},
	}
	return New(Options{
		Config: func() *config.Config { return cfg },
		Reply: func(_ context.Context, in model.Inbound, text string) error {
			if reply != nil {
				reply(text)
			}
			return nil
		},
		React: func(_ context.Context, in model.Inbound, emoji string) error {
			if react != nil {
				return react(emoji)
			}
			return nil
		},
		Builtins: []Builtin{
			{
				Satellite:       "remind",
				Trigger:         "remind",
				SuccessReaction: "👍",
				SuccessText:     "Reminder set.",
				Handle:          func(context.Context, model.Inbound, string) error { return nil },
			},
			{
				Satellite: "config",
				Trigger:   "config",
				Handle: func(context.Context, model.Inbound, string) error {
					return sdk.Userf("Usage: config set <key> <value>")
				},
			},
			{
				Satellite: "boom",
				Trigger:   "boom",
				Handle: func(context.Context, model.Inbound, string) error {
					return errors.New("internal detail")
				},
			},
		},
	})
}

func TestBuiltinSuccessReaction(t *testing.T) {
	var replies, reactions []string
	rt := feedbackRouter(func(text string) { replies = append(replies, text) }, func(emoji string) error {
		reactions = append(reactions, emoji)
		return nil
	})

	rt.Dispatch(context.Background(), model.Inbound{
		Sender:      "+15555550102",
		Text:        "!remind me stretch in 1 minute",
		MessageID:   "m1",
		TimestampMS: 1767614400000,
	})

	if len(reactions) != 1 || reactions[0] != "👍" {
		t.Fatalf("reactions = %v, want [👍]", reactions)
	}
	if len(replies) != 0 {
		t.Fatalf("replies = %v, want none on a successful reaction", replies)
	}
}

func TestBuiltinReactionFallsBackToText(t *testing.T) {
	var replies []string
	rt := feedbackRouter(func(text string) { replies = append(replies, text) }, func(string) error {
		return errors.New("provider has no reactions")
	})

	rt.Dispatch(context.Background(), model.Inbound{
		Sender: "+15555550102",
		Text:   "!remind me stretch in 1 minute",
	})

	if len(replies) != 1 || replies[0] != "Reminder set." {
		t.Fatalf("replies = %v, want [Reminder set.]", replies)
	}
}

func TestBuiltinErrors(t *testing.T) {
	t.Run("safe user error is sent verbatim", func(t *testing.T) {
		var replies []string
		rt := feedbackRouter(func(text string) { replies = append(replies, text) }, nil)

		rt.Dispatch(context.Background(), model.Inbound{
			Sender: "+15555550102",
			Text:   "!config",
		})

		if len(replies) != 1 || replies[0] != "Usage: config set <key> <value>" {
			t.Fatalf("replies = %v", replies)
		}
	})

	t.Run("internal error is generic", func(t *testing.T) {
		var replies []string
		rt := feedbackRouter(func(text string) { replies = append(replies, text) }, nil)

		rt.Dispatch(context.Background(), model.Inbound{
			Sender: "+15555550102",
			Text:   "!boom",
		})

		if len(replies) != 1 {
			t.Fatalf("replies = %v, want one", replies)
		}
		if strings.Contains(replies[0], "internal detail") {
			t.Fatalf("reply leaked the internal error: %q", replies[0])
		}
		if !strings.Contains(replies[0], "!help") {
			t.Fatalf("reply = %q, want a help hint", replies[0])
		}
	})
}

func TestUnknownCommandFeedback(t *testing.T) {
	t.Run("prefixed unknown command is answered", func(t *testing.T) {
		var replies []string
		rt := feedbackRouter(func(text string) { replies = append(replies, text) }, nil)

		rt.Dispatch(context.Background(), model.Inbound{
			Sender: "+15555550102",
			Text:   "!nope now",
		})

		if len(replies) != 1 || !strings.Contains(replies[0], "Unknown command") {
			t.Fatalf("replies = %v", replies)
		}
		if !strings.Contains(replies[0], "!help") {
			t.Fatalf("reply = %q, want a help hint", replies[0])
		}
	})

	t.Run("plain chat stays silent", func(t *testing.T) {
		var replies []string
		rt := feedbackRouter(func(text string) { replies = append(replies, text) }, nil)

		rt.Dispatch(context.Background(), model.Inbound{
			Sender: "+15555550102",
			Text:   "just a normal message",
		})

		if len(replies) != 0 {
			t.Fatalf("replies = %v, want none", replies)
		}
	})
}
