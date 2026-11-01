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
	for _, command := range route.Commands {
		spec := commandSpec(command)
		raw, matched := sdk.MatchCommand(canonical, spec)
		if !matched {
			continue
		}

		args, err := sdk.ConvertArgs(spec, raw, loc)
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

		envelope := transmission{
			Command:   command.Name,
			Trigger:   route.Trigger,
			Args:      args,
			Text:      canonical,
			Sender:    in.Sender,
			Satellite: route.Satellite,
		}
		if err := r.post(ctx, route.Endpoint, command.Path, envelope); err != nil {
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

		r.record(ctx, store.RouteLog{
			Direction: "inbound",
			Sender:    in.Sender,
			Satellite: route.Satellite,
			Text:      canonical,
			Status:    "dispatched",
		})
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
