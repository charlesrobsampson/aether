# Adding a satellite

A satellite is an HTTP service that receives typed commands from Aether and
replies through `/message`. It does not need to know about transports, JWTs for
inbound traffic, or the queue: it parses the dispatch envelope, does its work,
and posts a response envelope back.

The canonical contracts are in [../plan.md](../plan.md) §3.6, §3.11, and §3.15.
For the surrounding components, see [architecture.md](architecture.md).

## 1. Declare the satellite

Write `satellites/<name>.json` under `AETHER_CONFIG_DIR`, or register it over
`POST /register-satellite`. Registration requires an `admin` or `maintainer` JWT
and validates the document before writing it atomically, mirroring it into
SQLite, and rebuilding `routes.json`.

```json
{
  "name": "wind-predictor",
  "trigger": "fly",
  "endpoint": "http://127.0.0.1:9463",
  "prefix": "wind",
  "commands": [
    {
      "name": "fly",
      "path": "/api/v1/predict/text",
      "patterns": ["fly <station>"],
      "description": "Forecast for a station using your default profile",
      "args": [
        { "name": "station", "type": "string" }
      ]
    }
  ]
}
```

Rules enforced by registration:

- `name` matches `^[a-z0-9][a-z0-9-]*$`; it becomes the file name, so path
  separators and dot segments are rejected.
- `prefix`, when present, matches the same pattern. It is a short alias for
  addressing the satellite (`wind` instead of `wind-predictor`); the canonical
  name and file key remain valid.
- `name` and `trigger` must be unique, and neither may shadow the built-ins
  `config`, `admin`, `remind`, or `test` (`409` conflict).
- `endpoint` is an absolute `http` or `https` URL.
- Every command has a non-empty name, at least one pattern, and a path that
  starts with `/`.
- `description`, when present, is shown next to that command in `help`. Without
  it, help falls back to the satellite-level description. Declare one command
  per action so help can name each action explicitly instead of showing an
  opaque `<action>` placeholder.

`routes.json` is regenerated from all satellite files after every registration.
Never edit it by hand.

## 2. Write the patterns

Patterns match the full message **including the trigger**. Prefixes (`!`, `/`,
`Aether `) are stripped before matching, and satellite/command aliases from the
user file are resolved first. Patterns are anchored, whitespace runs match
`\s+`, and each `<arg>` becomes a lazy named group. Ambiguous patterns resolve in
declaration order, so put the most specific pattern first.

| `ArgDef.Type` | Behaviour |
|---|---|
| `string` | Raw text, trimmed. |
| `int` | Parsed as a base-10 integer. |
| `enum` | Must be one of the declared `enum` values. |
| `datetime` | Parsed in the user's timezone and converted to UTC. |

Supported datetime layouts, tried in order: RFC3339, `2006-01-02 15:04`,
`2006-01-02T15:04`, `15:04` (today in the user's timezone), and `Monday 15:04`
(next occurrence). `time/tzdata` is compiled in, so named zones resolve even in
containers without zoneinfo.

To handle a multi-step flow, declare a reply pattern that does not repeat the
trigger, for example `<choice>`. When a session is active, Aether matches
inbound text against that pattern and posts the standard envelope with
`thread_id` set. The user can target a specific thread with `#thread-id`.

## 3. Handle the dispatch envelope

Aether posts this JSON to `endpoint + path` with a five-second timeout:

```json
{
  "command": "fly",
  "trigger": "fly",
  "args": { "station": "FPS" },
  "text": "fly FPS",
  "sender": "+15555550102",
  "satellite": "wind-predictor",
  "thread_id": "optional"
}
```

`sdk.ParseWebhook` accepts only `POST` with `application/json` and validates that
the body is exactly one JSON object. `args` already holds converted values, so a
satellite never re-parses numbers or datetimes.

```go
func handle(w http.ResponseWriter, r *http.Request) {
	transmission, err := sdk.ParseWebhook(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	station, _ := transmission.Args["station"].(string)
	text := predictFor(station)

	sdk.SendResponse(w, sdk.Response{
		Recipients: []string{transmission.Sender},
		Text:       text,
		Satellite:  transmission.Satellite,
		ThreadID:   transmission.ThreadID,
	})
}
```

`SendResponse` writes the response envelope as JSON with the response's `Status`
(or `200` when unset). Aether converts the envelope to an outbound job; it
applies authentication, muting, rate limiting, and delivery.

`Recipients` and `GroupChatID` are optional. When both are empty, Aether replies
to the conversation that triggered the dispatch: the group for a group message,
otherwise the sender. `Satellite` and `ThreadID` likewise default to the route
and thread from the dispatch envelope, so a minimal `{"text": "..."}` response
is enough for a one-line answer.

Pass `Format: "styled"` to opt into provider-native formatting. Aether currently
forwards it to Signal, which renders `*bold*`, `_italic_`, `~strikethrough~`,
`||spoiler||`, and `` `monospace` ``. An empty format (or `"plain"`) sends
literal text; providers without styled support receive the text as-is.

## 4. Start, extend, and close sessions

Attach a session action to a response, or POST one directly to `/message`:

```go
resp := sdk.NewSessionManager("wind-predictor").
	Lock(transmission.Sender, transmission.ThreadID, 5*time.Minute)
resp.Text = "Pick a forecast: 1) today 2) tomorrow"
sdk.SendResponse(w, resp)
```

- `lock` starts a session with the given TTL; an empty thread ID defaults to the
  satellite name.
- `extend` resets the TTL on an existing session.
- `close` removes it.

While a session is active, the next message from that phone is routed to this
satellite before trigger parsing. Sessions are in memory only and never
persisted.

## 5. Push unsolicited messages

Satellites that deliver alerts (such as `wind-predictor`) call Aether directly:

```go
client := sdk.NewClient("http://127.0.0.1:8080", os.Getenv("AETHER_TOKEN"))
err := client.Send(ctx, sdk.Response{
	Recipients: []string{user.AetherRecipient},
	Text:       alertText,
	Satellite:  "wind-predictor",
})
```

`AETHER_TOKEN` must be an admin or maintainer JWT. The client returns an error
for any non-2xx response so the caller can record a failed delivery.

## 6. Configure users

A user file maps an Aether phone to the satellite's own identity:

```json
{
  "timezone": "America/Chicago",
  "satellite_ids": { "wind-predictor": "alice" }
}
```

A satellite can forward user-scoped RFC6902 patches to its own config endpoint
using this mapping. The role check always happens in Aether; schema validation
belongs to the satellite. `satellite_ids` is admin-managed and is not part of the
user `!config set` whitelist.

Users can address the satellite directly by prefixing `config` with its short
prefix (or canonical name):

```
!wind config list
!wind config get alert_schedule.nightly_local
!wind config export
!wind config set alert_schedule.nightly_local 21:45
```

Reads use `GET /api/v1/config?user=<id>` and expect a flat
`{"settings":[{"key":"...","value":"...","note":"..."}]}` response. A non-empty
`note` marks a default or read-only value, so `export` skips it and `get` does
not offer a `Set with:` line. Writes use `POST /api/v1/config` with a
user-scoped RFC6902 patch. `!wind config set ...` is equivalent to
`!config set wind-predictor.alert_schedule.nightly_local 21:45`. The unprefixed
`!config` remains Aether-level, so a satellite config change always needs the
prefix. Satellite triggers accept the same prefix (`!wind fly FPS`); it is
optional when the trigger is unique. Aether's built-ins accept the same shape
through `!aether <command>`; only built-ins resolve after that prefix. For
admins, `!help` lists the satellite config command under the satellite heading,
and `!help wind` lists that satellite's commands. Config reads and forwarding
are admin-only and must originate in a private chat; groups ignore them
silently.

## 7. Test it

Use `httptest` for the satellite and `internal/testutil` fakes in Aether. A
minimal satellite test:

```go
func TestFly(t *testing.T) {
	body := `{"command":"fly","trigger":"fly","args":{"station":"FPS"},` +
		`"text":"fly FPS","sender":"+15555550102","satellite":"wind-predictor"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/predict/text", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
```

Run the repository gate before opening a change:

```bash
go test ./...
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
```
