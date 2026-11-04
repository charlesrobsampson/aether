package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/model"
	"github.com/charlesrobsampson/aether/internal/session"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

// transmission is the JSON envelope Aether posts to a satellite (plan §3.6).
// Args carries the converted, typed values; the metadata fields let the
// satellite reply to the right user and thread.
type transmission struct {
	Command   string         `json:"command"`
	Trigger   string         `json:"trigger"`
	Args      map[string]any `json:"args"`
	Text      string         `json:"text"`
	Sender    string         `json:"sender"`
	Satellite string         `json:"satellite"`
	ThreadID  string         `json:"thread_id,omitempty"`
}

// Dispatch routes one accepted inbound message. Every outcome is written to
// route_log; failures are never returned to the sender and Dispatch never
// generates a reply (plan §3.6).
//
// An active session intercepts the message before trigger parsing (plan §3.7):
// the reply is matched against the session satellite's command patterns and
// posted to the matched command's path.
func (r *Router) Dispatch(ctx context.Context, in model.Inbound) {
	cfg := r.configSnapshot()
	if cfg == nil {
		r.record(ctx, store.RouteLog{
			Direction: "inbound",
			Sender:    in.Sender,
			Text:      in.Text,
			Status:    "dropped",
			Error:     "configuration unavailable",
		})
		return
	}

	user := cfg.Users[in.Sender]
	text := stripPrefixes(in.Text, cfg.System.Persona.Prefixes, user.Prefixes)
	if text == "" {
		r.record(ctx, store.RouteLog{
			Direction: "inbound",
			Sender:    in.Sender,
			Text:      text,
			Status:    "dropped",
			Error:     "empty message",
		})
		return
	}

	if sess, reply, ok := r.resolveSession(in.Sender, text); ok {
		r.dispatchSession(ctx, cfg, in, user, sess, reply)
		return
	}

	// Built-ins are internal handlers and take precedence over file
	// satellites with the same trigger (plan §3.6).
	if builtin, ok := r.resolveBuiltin(text); ok {
		r.dispatchBuiltin(ctx, in, builtin, text)
		return
	}

	route, canonical, ok := resolveRoute(cfg, user, text)
	if !ok {
		r.record(ctx, store.RouteLog{
			Direction: "inbound",
			Sender:    in.Sender,
			Text:      text,
			Status:    "dropped",
			Error:     "unknown trigger",
		})
		return
	}

	loc, err := userLocation(user.Timezone)
	if err != nil {
		r.record(ctx, store.RouteLog{
			Direction: "inbound",
			Sender:    in.Sender,
			Satellite: route.Satellite,
			Text:      canonical,
			Status:    "failed",
			Error:     err.Error(),
		})
		return
	}

	// Patterns include the trigger, so the canonical text (aliases resolved)
	// is what MatchCommand sees. Declaration order wins (plan §3.6).
	if r.dispatchRoute(ctx, in, route, loc, canonical, canonical, "") {
		return
	}

	r.record(ctx, store.RouteLog{
		Direction: "inbound",
		Sender:    in.Sender,
		Satellite: route.Satellite,
		Text:      canonical,
		Status:    "dropped",
		Error:     "no matching command",
	})
}

// resolveBuiltin returns the internal handler whose trigger is the first token
// of text. Built-ins are checked in registration order, so the app controls
// precedence.
func (r *Router) resolveBuiltin(text string) (Builtin, bool) {
	if r == nil {
		return Builtin{}, false
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return Builtin{}, false
	}
	for _, builtin := range r.builtins {
		if builtin.Trigger != "" && builtin.Trigger == fields[0] {
			return builtin, true
		}
	}
	return Builtin{}, false
}

// dispatchBuiltin runs one internal command and records its outcome. Built-in
// handlers answer through the outbound queue directly; a failure is audited and
// never returned to the transport.
func (r *Router) dispatchBuiltin(ctx context.Context, in model.Inbound, builtin Builtin, text string) {
	record := store.RouteLog{
		Direction: "inbound",
		Sender:    in.Sender,
		Satellite: builtin.Satellite,
		Text:      text,
		Status:    "dispatched",
	}
	if builtin.Handle == nil {
		record.Status = "failed"
		record.Error = "built-in handler is not configured"
		r.record(ctx, record)
		return
	}
	if err := builtin.Handle(ctx, in, text); err != nil {
		record.Status = "failed"
		record.Error = err.Error()
	}
	r.record(ctx, record)
}

// dispatchRoute posts the first command in route whose pattern matches
// matchText. envelopeText is what the satellite sees as text, and threadID is
// carried through when non-empty. It reports whether a command matched, whether
// or not the POST succeeded; the caller records a drop when nothing matched.
func (r *Router) dispatchRoute(ctx context.Context, in model.Inbound, route config.RouteEntry, loc *time.Location, matchText, envelopeText, threadID string) bool {
	for _, command := range route.Commands {
		spec := commandSpec(command)
		raw, matched := sdk.MatchCommand(matchText, spec)
		if !matched {
			continue
		}

		args, err := sdk.ConvertArgs(spec, raw, loc)
		if err != nil {
			r.record(ctx, store.RouteLog{
				Direction: "inbound",
				Sender:    in.Sender,
				Satellite: route.Satellite,
				Thread:    threadID,
				Text:      envelopeText,
				Status:    "failed",
				Error:     err.Error(),
			})
			return true
		}

		envelope := transmission{
			Command:   command.Name,
			Trigger:   route.Trigger,
			Args:      args,
			Text:      envelopeText,
			Sender:    in.Sender,
			Satellite: route.Satellite,
			ThreadID:  threadID,
		}
		if err := r.post(ctx, route.Endpoint, command.Path, envelope); err != nil {
			r.record(ctx, store.RouteLog{
				Direction: "inbound",
				Sender:    in.Sender,
				Satellite: route.Satellite,
				Thread:    threadID,
				Text:      envelopeText,
				Status:    "failed",
				Error:     err.Error(),
			})
			return true
		}

		r.record(ctx, store.RouteLog{
			Direction: "inbound",
			Sender:    in.Sender,
			Satellite: route.Satellite,
			Thread:    threadID,
			Text:      envelopeText,
			Status:    "dispatched",
		})
		return true
	}
	return false
}

// dispatchSession sends an intercepted reply to the session's satellite. The
// reply is matched against that satellite's configured command patterns; a
// satellite therefore declares a reply pattern (for example "<choice>") on the
// command that should handle session input. No trigger is involved.
func (r *Router) dispatchSession(ctx context.Context, cfg *config.Config, in model.Inbound, user config.UserConfig, sess session.Session, reply string) {
	if strings.TrimSpace(reply) == "" {
		r.record(ctx, store.RouteLog{
			Direction: "inbound",
			Sender:    in.Sender,
			Satellite: sess.Satellite,
			Thread:    sess.ThreadID,
			Text:      reply,
			Status:    "dropped",
			Error:     "empty message",
		})
		return
	}

	route, ok := routeBySatellite(cfg, sess.Satellite)
	if !ok {
		r.record(ctx, store.RouteLog{
			Direction: "inbound",
			Sender:    in.Sender,
			Satellite: sess.Satellite,
			Thread:    sess.ThreadID,
			Text:      reply,
			Status:    "dropped",
			Error:     "unknown session satellite",
		})
		return
	}

	loc, err := userLocation(user.Timezone)
	if err != nil {
		r.record(ctx, store.RouteLog{
			Direction: "inbound",
			Sender:    in.Sender,
			Satellite: route.Satellite,
			Thread:    sess.ThreadID,
			Text:      reply,
			Status:    "failed",
			Error:     err.Error(),
		})
		return
	}

	if r.dispatchRoute(ctx, in, route, loc, reply, reply, sess.ThreadID) {
		return
	}

	r.record(ctx, store.RouteLog{
		Direction: "inbound",
		Sender:    in.Sender,
		Satellite: route.Satellite,
		Thread:    sess.ThreadID,
		Text:      reply,
		Status:    "dropped",
		Error:     "no matching command",
	})
}

// resolveSession finds the live session that should receive text. A leading
// "#thread-id" token selects that thread; otherwise the phone's most recently
// active session wins (plan §3.7). The returned reply has the tag removed.
func (r *Router) resolveSession(phone, text string) (session.Session, string, bool) {
	if r == nil || r.sessions == nil {
		return session.Session{}, "", false
	}

	if thread, reply, tagged := splitThreadTag(text); tagged {
		for _, sess := range r.sessions.List(phone) {
			if sess.ThreadID == thread {
				return sess, reply, true
			}
		}
		return session.Session{}, "", false
	}

	if sess, ok := r.sessions.MostRecent(phone); ok {
		return sess, text, true
	}
	return session.Session{}, "", false
}

// splitThreadTag parses a leading "#thread-id" token. It reports false when the
// text has no tag, so the caller can fall through to trigger parsing.
func splitThreadTag(text string) (thread, reply string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "#") {
		return "", "", false
	}

	fields := strings.Fields(text)
	thread = strings.TrimPrefix(fields[0], "#")
	if thread == "" {
		return "", "", false
	}
	return thread, strings.TrimSpace(strings.TrimPrefix(text, fields[0])), true
}

// routeBySatellite returns the route whose satellite name matches.
func routeBySatellite(cfg *config.Config, satellite string) (config.RouteEntry, bool) {
	for _, route := range routesFromConfig(cfg) {
		if route.Satellite == satellite {
			return route, true
		}
	}
	return config.RouteEntry{}, false
}

// ApplySessionAction applies a lock, extend, or close action to the session
// identified by phone/satellite/thread. An empty thread defaults to the
// satellite name (plan §3.3), an empty action is a no-op, and an unknown action
// is rejected. It is called by the /message handler.
func (r *Router) ApplySessionAction(phone, satellite, thread string, action model.SessionAction) error {
	switch action.Action {
	case "":
		return nil
	case "lock", "extend", "close":
	default:
		return fmt.Errorf("router: unknown session action %q", action.Action)
	}

	if r == nil || r.sessions == nil {
		return nil
	}

	if thread == "" {
		thread = satellite
	}
	ttl := session.DefaultTTL
	if action.TTLSeconds > 0 {
		ttl = time.Duration(action.TTLSeconds) * time.Second
	}

	switch action.Action {
	case "lock":
		r.sessions.StartWithTTL(phone, satellite, thread, ttl)
	case "extend":
		if !r.sessions.Extend(phone, satellite, thread, ttl) {
			// A satellite that re-sends extend after a restart should not
			// lose the thread, so a missing session is started instead.
			r.sessions.StartWithTTL(phone, satellite, thread, ttl)
		}
	case "close":
		r.sessions.Close(phone, satellite, thread)
	}
	return nil
}

// configSnapshot returns the active configuration, if any.
func (r *Router) configSnapshot() *config.Config {
	if r == nil || r.config == nil {
		return nil
	}
	return r.config()
}

// record writes one audit row. Audit failures are deliberately swallowed: a
// logging problem must not change message handling.
func (r *Router) record(ctx context.Context, rec store.RouteLog) {
	if r == nil || r.store == nil {
		return
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	_ = r.store.LogRoute(ctx, rec)
}

// resolveRoute selects the route named by the first token and returns the
// canonical command text (the trigger followed by the remaining words).
//
// Satellite aliases are resolved before the global trigger lookup, and command
// aliases map an abbreviation onto a trigger (plan §3.6).
func resolveRoute(cfg *config.Config, user config.UserConfig, text string) (config.RouteEntry, string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return config.RouteEntry{}, text, false
	}

	token := fields[0]
	routes := routesFromConfig(cfg)

	var route *config.RouteEntry
	if target := user.Aliases.Satellites[token]; target != "" {
		for i := range routes {
			if routes[i].Satellite == target {
				route = &routes[i]
				break
			}
		}
	}
	if route == nil {
		lookup := token
		if target := user.Aliases.Commands[token]; target != "" {
			lookup = target
		}
		for i := range routes {
			if routes[i].Trigger == lookup {
				route = &routes[i]
				break
			}
		}
	}
	if route == nil {
		return config.RouteEntry{}, text, false
	}

	canonical := route.Trigger
	if len(fields) > 1 {
		canonical += " " + strings.Join(fields[1:], " ")
	}
	return *route, canonical, true
}

// routesFromConfig returns routes.json's entries. When routes.json is absent the
// source satellites/*.json documents are used, because routes.json is a derived
// cache (docs/open-questions.md #7).
func routesFromConfig(cfg *config.Config) []config.RouteEntry {
	if cfg == nil {
		return nil
	}
	if len(cfg.Routes.Routes) > 0 {
		return cfg.Routes.Routes
	}

	names := make([]string, 0, len(cfg.Satellites))
	for name := range cfg.Satellites {
		names = append(names, name)
	}
	sort.Strings(names)

	routes := make([]config.RouteEntry, 0, len(names))
	for _, name := range names {
		satellite := cfg.Satellites[name]
		if satellite.Name == "" {
			satellite.Name = name
		}
		routes = append(routes, config.RouteEntry{
			Trigger:   satellite.Trigger,
			Satellite: satellite.Name,
			Endpoint:  satellite.Endpoint,
			Commands:  satellite.Commands,
		})
	}
	return routes
}

// stripPrefixes removes any configured prefix from the front of text. Prefixes
// are tried in order within each group; system prefixes are supplied first.
func stripPrefixes(text string, groups ...[]string) string {
	text = strings.TrimSpace(text)
	for {
		stripped := false
		for _, prefixes := range groups {
			for _, prefix := range prefixes {
				if prefix == "" || !strings.HasPrefix(text, prefix) {
					continue
				}
				text = strings.TrimSpace(text[len(prefix):])
				stripped = true
				break
			}
			if stripped {
				break
			}
		}
		if !stripped {
			return text
		}
	}
}

// commandSpec adapts a config command to the SDK parser's representation.
func commandSpec(command config.CommandConfig) sdk.CommandSpec {
	spec := sdk.CommandSpec{
		Name:     command.Name,
		Path:     command.Path,
		Patterns: command.Patterns,
		Args:     make([]sdk.ArgDef, len(command.Args)),
	}
	for i, arg := range command.Args {
		spec.Args[i] = sdk.ArgDef{Name: arg.Name, Type: arg.Type, Enum: arg.Enum}
	}
	return spec
}

// userLocation resolves the user's timezone for datetime conversion. An empty
// timezone means UTC.
func userLocation(timezone string) (*time.Location, error) {
	if strings.TrimSpace(timezone) == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("router: load timezone %q: %w", timezone, err)
	}
	return loc, nil
}

// post sends the dispatch envelope to endpoint+path. Any non-2xx response or
// transport error is returned for the audit row; the caller never replies to
// the sender.
func (r *Router) post(ctx context.Context, endpoint, path string, envelope transmission) error {
	url := strings.TrimRight(endpoint, "/")
	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	url += path

	raw, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("router: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("router: build POST %s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := r.client
	if client == nil {
		client = &http.Client{Timeout: defaultClientTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("router: POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("router: POST %s: status %d", url, resp.StatusCode)
	}
	return nil
}
