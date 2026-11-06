package transport

import (
	"context"
	"errors"
	"testing"
	"time"

	msgclient "github.com/charlesrobsampson/msg/client"

	"github.com/charlesrobsampson/aether/internal/config"
)

// fakeMsgMessenger stands in for msg/client.Messenger so the adapter test never
// touches a provider, the network, or $HOME.
type fakeMsgMessenger struct {
	name  string
	calls []fakeMsgCall
	err   error
}

type fakeMsgCall struct {
	ctx       context.Context
	recipient string
	text      string
}

func (f *fakeMsgMessenger) Send(ctx context.Context, recipient, text string) error {
	f.calls = append(f.calls, fakeMsgCall{ctx: ctx, recipient: recipient, text: text})
	return f.err
}

func (f *fakeMsgMessenger) Name() string { return f.name }

type fakeContextKey struct{}

func TestMsgAdapter(t *testing.T) {
	var _ msgclient.Messenger = (*fakeMsgMessenger)(nil)

	t.Run("forwards send unchanged", func(t *testing.T) {
		fake := &fakeMsgMessenger{name: "signal"}
		adapter, err := newMsgAdapter(fake)
		if err != nil {
			t.Fatalf("newMsgAdapter() error = %v", err)
		}
		var _ Messenger = adapter
		if got := adapter.Name(); got != "signal" {
			t.Fatalf("Name() = %q, want %q", got, "signal")
		}

		ctx := context.WithValue(context.Background(), fakeContextKey{}, "carried")
		if err := adapter.Send(ctx, "+15555550102", "hello from msg"); err != nil {
			t.Fatalf("Send() error = %v, want nil", err)
		}
		if got, want := len(fake.calls), 1; got != want {
			t.Fatalf("inner Send calls = %d, want %d", got, want)
		}
		call := fake.calls[0]
		if call.recipient != "+15555550102" {
			t.Errorf("recipient = %q, want %q", call.recipient, "+15555550102")
		}
		if call.text != "hello from msg" {
			t.Errorf("text = %q, want %q", call.text, "hello from msg")
		}
		if got := call.ctx.Value(fakeContextKey{}); got != "carried" {
			t.Errorf("context value = %v, want %q", got, "carried")
		}
	})

	t.Run("propagates errors", func(t *testing.T) {
		sentinel := errors.New("provider unavailable")
		adapter, err := newMsgAdapter(&fakeMsgMessenger{name: "signal", err: sentinel})
		if err != nil {
			t.Fatalf("newMsgAdapter() error = %v", err)
		}
		if err := adapter.Send(context.Background(), "+15555550102", "hi"); !errors.Is(err, sentinel) {
			t.Fatalf("Send() error = %v, want %v", err, sentinel)
		}
	})

	t.Run("maps provider config", func(t *testing.T) {
		var got msgclient.MessengerConfig
		factory := func(cfg msgclient.MessengerConfig) (msgclient.Messenger, error) {
			got = cfg
			return &fakeMsgMessenger{name: cfg.Provider}, nil
		}

		dbPath := t.TempDir() + "/signal.db"
		m, err := newMsgMessenger("signal", config.ProviderConfig{
			Type:    "msg",
			Account: "+15555550100",
			BaseURL: "http://127.0.0.1:9999",
			DBPath:  dbPath,
		}, factory)
		if err != nil {
			t.Fatalf("newMsgMessenger() error = %v", err)
		}
		want := msgclient.MessengerConfig{
			Provider: "signal",
			Account:  "+15555550100",
			BaseURL:  "http://127.0.0.1:9999",
			DBPath:   dbPath,
		}
		if got != want {
			t.Fatalf("MessengerConfig = %#v, want %#v", got, want)
		}
		if m.Name() != "signal" {
			t.Fatalf("Name() = %q, want %q", m.Name(), "signal")
		}
	})

	t.Run("rejects a non msg provider type", func(t *testing.T) {
		_, err := newMsgMessenger("signal", config.ProviderConfig{Type: "http"}, func(msgclient.MessengerConfig) (msgclient.Messenger, error) {
			t.Fatal("factory called for a non-msg provider")
			return nil, nil
		})
		if err == nil {
			t.Fatalf("newMsgMessenger() accepted type %q, want error", "http")
		}
	})

	t.Run("rejects an unknown msg provider", func(t *testing.T) {
		m, err := NewMsgMessenger("telegram", config.ProviderConfig{Type: "msg"})
		if err == nil {
			t.Fatal("NewMsgMessenger() accepted an unknown msg provider")
		}
		if m != nil {
			t.Fatalf("NewMsgMessenger() = %v, want nil messenger", m)
		}
	})

	t.Run("wraps factory errors", func(t *testing.T) {
		sentinel := errors.New("unknown provider")
		_, err := newMsgMessenger("carrier-pigeon", config.ProviderConfig{Type: "msg"}, func(msgclient.MessengerConfig) (msgclient.Messenger, error) {
			return nil, sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("newMsgMessenger() error = %v, want wrapped %v", err, sentinel)
		}
	})
}

// TestStatusErrorMapping proves provider HTTP status metadata survives the
// adapter so the queue can classify retries in A05.
func TestStatusErrorMapping(t *testing.T) {
	providerErr := &msgclient.StatusError{
		Provider:   "signal",
		StatusCode: 429,
		RetryAfter: 2 * time.Second,
		Body:       "slow down",
	}
	adapter, err := newMsgAdapter(&fakeMsgMessenger{name: "signal", err: providerErr})
	if err != nil {
		t.Fatalf("newMsgAdapter() error = %v", err)
	}

	err = adapter.Send(context.Background(), "+15555550102", "hi")
	var sendErr *SendError
	if !errors.As(err, &sendErr) {
		t.Fatalf("Send() error = %v, want *SendError", err)
	}
	if sendErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", sendErr.StatusCode)
	}
	if sendErr.RetryAfter != 2*time.Second {
		t.Errorf("RetryAfter = %s, want 2s", sendErr.RetryAfter)
	}
	if !errors.Is(err, providerErr) {
		t.Errorf("Send() error does not unwrap to the provider error")
	}
}
