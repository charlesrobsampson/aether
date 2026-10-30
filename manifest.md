# AETHER ARCHITECTURE & DESIGN MANIFEST
Version: 1.0.0

> **Superseded where it conflicts with `plan.md`.** `plan.md` is the canonical
> source of truth and the checklist is the execution order. In particular, the
> manifest's YAML config, `/send` endpoint, scratch container, and
> `github.com/user/aether` module path are historical; see `plan.md` §1.1, §3,
> and §6.
Name: Aether
Core Engine: Go
Storage: SQLite (CGO-free via modernc.org/sqlite) + In-Memory TTL Session Store
Primary Transport: Signal CLI (Provider-Agnostic via Messenger Interface)

===============================================================================
1. PROJECT OVERVIEW
===============================================================================
Aether is a lightweight, self-hosted, config-driven messaging router that 
decouples communication platforms (Signal, Email, Matrix, Webhooks, Kafka) from 
personal automation services ("Satellites"). 

It acts as a central spaceport hub: receiving inbound messages, parsing CLI-style 
commands, maintaining stateful multi-step conversation threads, and routing 
payloads to external micro-services. Satellites can also send asynchronous 
webhooks back to Aether to trigger outbound alerts without waiting for an 
incoming request.

===============================================================================
2. SYSTEM ARCHITECTURE & COMPONENTS
===============================================================================

  +------------------------------------------------------------------------+
  |                            TRANSPORT LAYER                             |
  |  (Signal CLI / Matrix Bridge / Email SMTP / Webhooks / Kafka Broker)   |
  +-----------------------------------┬------------------------------------+
                                      │
                                      ▼
  +------------------------------------------------------------------------+
  |                              AETHER CORE                               |
  |                                                                        |
  |  +--------------------+   +--------------------+   +----------------+  |
  |  |  Inbound Receiver  |-->| Routing & Session  |-->| Outbound Queue |  |
  |  |   (HTTP Webhook)   |   |   Engine (SQLite)  |   |   & Dispatch   |  |
  |  +--------------------+   +--------------------+   +-------┬--------+  |
  +------------------------------------------------------------│-----------+
                                                               │
                                                               ▼
  +------------------------------------------------------------------------+
  |                           SATELLITE SERVICES                           |
  |   (Hang Gliding Alert, Jellyfin Launcher, Daily Outlook Scheduler)    |
  +------------------------------------------------------------------------+

===============================================================================
3. CORE FEATURES & SPECIFICATIONS
===============================================================================

3.1 Provider-Agnostic Messaging Interface
-------------------------------------------------------------------------------
The core router uses a Go interface for outbound delivery, allowing seamless 
swapping or multiplexing of messaging backends.

```go
type Messenger interface {
    Send(ctx context.Context, recipient string, message string) error
}
```

3.2 Config-Driven Command Routing
-------------------------------------------------------------------------------
- Strips configured bot names/prefixes (`!`, `/`, `Aether `).
- Matches keywords to remote satellite HTTP webhooks.
- Extracts trailing text arguments and formats them into standard JSON payloads.

3.3 Multi-Step Stateful Threading (Multiplexing)
-------------------------------------------------------------------------------
- Manages active conversation sessions using an in-memory KV store with configurable TTLs (e.g., 5-minute inactivity timeouts).
- Supports targeting concurrent multi-step flows via `#thread-id` tags (e.g., `#jellyfin 2`) or defaults to the most recently active thread.
- Standard flow:
  1. User texts `!watch sci-fi`.
  2. Router dispatches request to Jellyfin Satellite.
  3. Satellite responds with movie list and signals Aether to lock session state.
  4. User replies `2`.
  5. Router intercepts `2`, identifies active Jellyfin session, and routes directly back to Jellyfin Satellite.
  6. Satellite triggers playback and closes session state.

3.4 Asynchronous Outbound Queue
-------------------------------------------------------------------------------
- Provides an HTTP POST `/send` endpoint for satellite-initiated alerts (e.g., condition checks, daily briefings).
- Decouples micro-services from transport rate limits and signal-cli execution locks.

3.5 Built-In Cron Scheduler
-------------------------------------------------------------------------------
- Internal lightweight scheduler triggers satellite endpoints on cron schedules (e.g., daily outlooks at 7:00 AM).

===============================================================================
4. DELIVERABLES
===============================================================================

Deliverable 1: Core Hub Executable (`aether` binary)
-------------------------------------------------------------------------------
- Single compiled Go binary.
- Pure-Go embedded SQLite database for route logging and persistent audit trails.
- HTTP Webhook server for inbound transport and satellite communication.
- Asynchronous outbound message worker queue.

Deliverable 2: Satellite SDK (`pkg/sdk`)
-------------------------------------------------------------------------------
Importable Go package (`github.com/user/aether/pkg/sdk`) to quickly build 
compatible satellites:
- `sdk.ParseWebhook(r *http.Request) (*sdk.Transmission, error)`: Parses incoming payloads from Aether.
- `sdk.SendResponse(w http.ResponseWriter, resp sdk.Response)`: Formats and returns response payloads back to Aether.
- `sdk.SessionManager`: Helper routines to start, extend, or terminate multi-step conversation states.

Deliverable 3: System Configuration Schema (`config.yaml`)
-------------------------------------------------------------------------------
```yaml
server:
  port: 8080
  db_path: "./data/aether.db"

persona:
  name: "Aether"
  prefixes: ["!", "/", "Aether "]

providers:
  default: "signal"
  signal:
    type: "cmd"
    command: "signal-cli"
    args: ["-u", "+1234567890", "send"]
  email:
    type: "smtp"
    host: "smtp.example.com"
    port: 587
    user: "alerts@example.com"

routes:
  - keyword: "fly"
    service_url: "http://localhost:5001/check-conditions"
    extract_args: true
    default_provider: "signal"

  - keyword: "watch"
    service_url: "http://localhost:5002/jellyfin"
    extract_args: true
    default_provider: "signal"

schedules:
  - name: "morning_briefing"
    cron: "0 7 * * 1-5"
    webhook: "http://localhost:5003/daily-outlook"
```

Deliverable 4: Reference Satellites (`/examples`)
-------------------------------------------------------------------------------
- `examples/hang-gliding`: Proactive weather alert satellite pushing notifications when flyability conditions turn green.
- `examples/jellyfin`: Interactive multi-step satellite showing thread locking (`!watch` -> option `2` -> start stream).

Deliverable 5: Deployment Assets
-------------------------------------------------------------------------------
- Multi-stage `Dockerfile` producing a minimal Alpine/Scratch container.
- `docker-compose.yml` boilerplate running `aether` alongside `signal-cli`.