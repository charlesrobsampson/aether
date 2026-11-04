package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/charlesrobsampson/aether/internal/config"
	"github.com/charlesrobsampson/aether/internal/scheduler"
	"github.com/charlesrobsampson/aether/internal/store"
	"github.com/charlesrobsampson/aether/pkg/sdk"
)

// scheduleDispatcher posts a recurring system.json schedule to its satellite.
// On fire Aether dispatches the named command as if it came from the first
// configured recipient (plan §3.13); replies return through /message.
type scheduleDispatcher struct {
	config func() *config.Config
	store  *store.Store
	client *http.Client
}

// registerSchedules arms every system.json schedule on sched. Invalid cron
// specs fail startup rather than being silently skipped.
func registerSchedules(sched *scheduler.Scheduler, cfg *config.Config, dispatch func(config.ScheduleConfig)) ([]scheduler.ID, error) {
	ids := make([]scheduler.ID, 0, len(cfg.System.Schedules))
	for _, schedule := range cfg.System.Schedules {
		schedule := schedule
		if strings.TrimSpace(schedule.Cron) == "" {
			return ids, fmt.Errorf("app: schedule %q has no cron", schedule.Name)
		}
		id, err := sched.Every(schedule.Cron, func() { dispatch(schedule) })
		if err != nil {
			return ids, fmt.Errorf("app: schedule %q: %w", schedule.Name, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// dispatch runs one schedule and audit-logs the outcome.
func (d *scheduleDispatcher) dispatch(schedule config.ScheduleConfig) {
	cfg := d.config()
	if cfg == nil {
		d.log(schedule, "dropped", "configuration unavailable")
		return
	}

	route, command, ok := scheduleRoute(cfg, schedule)
	if !ok {
		d.log(schedule, "dropped", "unknown schedule route")
		return
	}

	args := schedule.Args
	if args == nil {
		args = map[string]any{}
	}
	envelope := sdk.Transmission{
		Command:   command.Name,
		Trigger:   route.Trigger,
		Args:      args,
		Text:      command.Name,
		Sender:    firstRecipient(schedule.Recipients),
		Satellite: route.Satellite,
	}
	if schedule.Command != "" {
		envelope.Command = schedule.Command
	}

	if err := d.post(route.Endpoint, command.Path, envelope); err != nil {
		d.log(schedule, "failed", err.Error())
		return
	}
	d.log(schedule, "dispatched", "")
}

// scheduleRoute finds the satellite/command named by a schedule. routes.json is
// a derived cache, so satellite files are the fallback.
func scheduleRoute(cfg *config.Config, schedule config.ScheduleConfig) (config.RouteEntry, config.CommandConfig, bool) {
	for _, route := range routesForSchedule(cfg) {
		if route.Satellite != schedule.Satellite && route.Trigger != schedule.Satellite {
			continue
		}
		for _, command := range route.Commands {
			if schedule.Command == "" || command.Name == schedule.Command {
				return route, command, true
			}
		}
	}
	return config.RouteEntry{}, config.CommandConfig{}, false
}

// routesForSchedule prefers routes.json and falls back to the satellite files.
func routesForSchedule(cfg *config.Config) []config.RouteEntry {
	if len(cfg.Routes.Routes) > 0 {
		return cfg.Routes.Routes
	}

	names := make([]string, 0, len(cfg.Satellites))
	for name := range cfg.Satellites {
		names = append(names, name)
	}
	sort.Strings(names)

	routes := make([]config.RouteEntry, 0, len(names))
	for _, key := range names {
		satellite := cfg.Satellites[key]
		if satellite.Name == "" {
			satellite.Name = key
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

// post sends the standard dispatch envelope to endpoint+path.
func (d *scheduleDispatcher) post(endpoint, path string, envelope sdk.Transmission) error {
	url := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if url == "" {
		return fmt.Errorf("app: schedule satellite has no endpoint")
	}
	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	url += path

	raw, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("app: marshal schedule payload: %w", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("app: build schedule request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := d.client
	if client == nil {
		client = &http.Client{Timeout: dispatchTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("app: POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("app: POST %s: status %d", url, resp.StatusCode)
	}
	return nil
}

// log writes one outbound audit row. Audit failures never change scheduling.
func (d *scheduleDispatcher) log(schedule config.ScheduleConfig, status, detail string) {
	if d == nil || d.store == nil {
		return
	}
	_ = d.store.LogRoute(context.Background(), store.RouteLog{
		Direction: "outbound",
		Recipient: strings.Join(schedule.Recipients, ","),
		Satellite: schedule.Satellite,
		Text:      schedule.Command,
		Status:    status,
		Error:     detail,
	})
}

// firstRecipient returns the originating recipient for the envelope, or an
// empty sender when the schedule has none.
func firstRecipient(recipients []string) string {
	if len(recipients) == 0 {
		return ""
	}
	return recipients[0]
}
