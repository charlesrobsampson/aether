package transport

import (
	"context"
	"errors"
	"fmt"
	"strings"

	msgclient "github.com/charlesrobsampson/msg/client"

	"github.com/charlesrobsampson/aether/internal/config"
)

// msgFactory builds a msg/client.Messenger. It is a variable-sized seam so the
// config mapping can be tested without constructing a real provider.
type msgFactory func(msgclient.MessengerConfig) (msgclient.Messenger, error)

// msgAdapter adapts msg/client.Messenger to Aether's Messenger interface.
type msgAdapter struct {
	inner msgclient.Messenger
}

// NewMsgMessenger builds the outbound Messenger for a `providers.<name>` entry
// whose type is "msg". name is the provider key from system.json ("signal" or
// "openmessage"); the remaining fields map directly onto msg's MessengerConfig.
func NewMsgMessenger(name string, cfg config.ProviderConfig) (Messenger, error) {
	return newMsgMessenger(name, cfg, msgclient.NewMessenger)
}

func newMsgMessenger(name string, cfg config.ProviderConfig, factory msgFactory) (Messenger, error) {
	if !strings.EqualFold(strings.TrimSpace(cfg.Type), "msg") {
		return nil, fmt.Errorf("transport: provider %q has type %q, want %q", name, cfg.Type, "msg")
	}
	inner, err := factory(msgclient.MessengerConfig{
		Provider: name,
		Account:  cfg.Account,
		BaseURL:  cfg.BaseURL,
		DBPath:   cfg.DBPath,
	})
	if err != nil {
		return nil, fmt.Errorf("transport: build msg provider %q: %w", name, err)
	}
	return newMsgAdapter(inner)
}

func newMsgAdapter(inner msgclient.Messenger) (Messenger, error) {
	if inner == nil {
		return nil, fmt.Errorf("transport: nil msg messenger")
	}
	return &msgAdapter{inner: inner}, nil
}

// Send forwards the context, recipient, and text to the wrapped msg messenger
// and maps a typed provider status onto SendError so retry classification does
// not depend on error strings.
func (m *msgAdapter) Send(ctx context.Context, recipient, text string) error {
	err := m.inner.Send(ctx, recipient, text)
	if err == nil {
		return nil
	}
	var statusErr *msgclient.StatusError
	if errors.As(err, &statusErr) {
		return &SendError{
			StatusCode: statusErr.StatusCode,
			RetryAfter: statusErr.RetryAfter,
			Err:        err,
		}
	}
	return err
}

// React forwards a reaction to the wrapped msg messenger and normalizes the
// provider's unsupported-reaction sentinel so callers can fall back to text
// without importing msg.
func (m *msgAdapter) React(ctx context.Context, reaction Reaction) error {
	err := m.inner.React(ctx, msgclient.ReactionRequest{
		Recipient:         reaction.Recipient,
		TargetAuthor:      reaction.TargetAuthor,
		TargetTimestampMS: reaction.TargetTimestampMS,
		Emoji:             reaction.Emoji,
		Remove:            reaction.Remove,
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, msgclient.ErrReactionsUnsupported) {
		return ErrReactionsUnsupported
	}
	var statusErr *msgclient.StatusError
	if errors.As(err, &statusErr) {
		return &SendError{
			StatusCode: statusErr.StatusCode,
			RetryAfter: statusErr.RetryAfter,
			Err:        err,
		}
	}
	return err
}

// Name reports the underlying msg provider name.
func (m *msgAdapter) Name() string { return m.inner.Name() }
