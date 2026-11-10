// Package transport defines Aether's outbound delivery abstraction.
//
// A Messenger is the narrow seam between the outbound queue and a concrete
// provider (currently the msg library). Tests use Fake instead of a real
// provider, so nothing in this package touches the network or $HOME.
package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// ErrReactionsUnsupported is returned by a Messenger whose provider cannot
// attach reactions to existing messages. Callers may fall back to a text
// acknowledgement.
var ErrReactionsUnsupported = errors.New("transport: provider does not support reactions")

// Reaction targets one existing inbound message.
//
// Recipient is the conversation that holds the message (a phone number for a
// direct chat, a group ID for a group). TargetAuthor and TargetTimestampMS
// identify the specific message and come from the inbound envelope.
type Reaction struct {
	Recipient         string
	TargetAuthor      string
	TargetTimestampMS int64
	Emoji             string
	Remove            bool
}

// Messenger delivers a text message to a single recipient, and can optionally
// attach a reaction to an existing message.
//
// Recipient is either a phone number or a provider-specific conversation ID.
// Send must return an error when delivery fails; callers treat a nil error as a
// confirmed hand-off to the provider. React returns ErrReactionsUnsupported
// when the provider has no reaction support.
type Messenger interface {
	Send(ctx context.Context, recipient, text string) error
	React(ctx context.Context, reaction Reaction) error
	Name() string
}

// StyledMessenger is an optional capability. A provider that can render
// provider-native formatting implements it; the queue falls back to plain Send
// for providers that do not.
type StyledMessenger interface {
	SendStyled(ctx context.Context, recipient, text string, styled bool) error
}

// SendError carries provider status metadata so the queue can decide whether a
// failure is retryable without parsing provider strings. Err is the underlying
// provider error, when there is one.
type SendError struct {
	StatusCode int
	RetryAfter time.Duration
	Err        error
}

func (e *SendError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("transport: provider returned HTTP %d", e.StatusCode)
	}
	return "transport: provider send failed"
}

func (e *SendError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Retryable classifies an outbound failure. It reports whether another attempt
// is worthwhile and, when the provider supplied one, the minimum delay from
// Retry-After.
//
// Only network/context errors and provider 429/5xx statuses are retryable.
// Every other positive provider status (400, 401, 403, 404, 409, ...) is
// terminal, and an unrecognised error is treated as terminal too.
func Retryable(err error) (bool, time.Duration) {
	if err == nil {
		return false, 0
	}

	var sendErr *SendError
	if errors.As(err, &sendErr) && sendErr.StatusCode > 0 {
		switch {
		case sendErr.StatusCode == http.StatusTooManyRequests:
			return true, sendErr.RetryAfter
		case sendErr.StatusCode >= 500 && sendErr.StatusCode <= 599:
			return true, sendErr.RetryAfter
		default:
			return false, 0
		}
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true, 0
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true, 0
	}

	return false, 0
}

// Call is one recorded Send invocation on a Fake.
type Call struct {
	Recipient string
	Text      string
	Styled    bool
}

// ReactionCall records one Fake.React attempt.
type ReactionCall struct {
	Reaction Reaction
}

// Fake is an in-memory Messenger that records every Send and can be told to
// fail. It is safe for concurrent use.
type Fake struct {
	mu            sync.Mutex
	calls         []Call
	reactionCalls []ReactionCall
	err           error
	reactionErr   error
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{}
}

// Send records the attempt and returns the error most recently installed by
// FailWith. The attempt is recorded even when it fails.
func (f *Fake) Send(ctx context.Context, recipient, text string) error {
	return f.SendStyled(ctx, recipient, text, false)
}

// SendStyled records the attempt, including whether styled text was requested.
func (f *Fake) SendStyled(_ context.Context, recipient, text string, styled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Recipient: recipient, Text: text, Styled: styled})
	return f.err
}

// React records the attempt and returns the error most recently installed by
// FailReactionsWith.
func (f *Fake) React(_ context.Context, reaction Reaction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactionCalls = append(f.reactionCalls, ReactionCall{Reaction: reaction})
	return f.reactionErr
}

// Name identifies the fake provider.
func (f *Fake) Name() string {
	return "fake"
}

// Calls returns a copy of every recorded Send, in invocation order. Failed
// attempts are included.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Call, len(f.calls))
	copy(out, f.calls)
	return out
}

// ReactionCalls returns a copy of every recorded React, in invocation order.
func (f *Fake) ReactionCalls() []ReactionCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ReactionCall, len(f.reactionCalls))
	copy(out, f.reactionCalls)
	return out
}

// FailWith makes subsequent Send calls return err. Passing nil clears the
// failure and restores successful sends.
func (f *Fake) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// FailReactionsWith makes subsequent React calls return err. Passing nil
// clears the failure and restores successful reactions.
func (f *Fake) FailReactionsWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactionErr = err
}
