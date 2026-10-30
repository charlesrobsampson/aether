# Aether

Aether is a central messaging router that sits between a messenger (`msg`) and
satellite services such as `wind-predictor`. It receives inbound messages,
resolves command triggers to satellites, manages short-lived multi-step sessions,
and delivers outbound replies through a rate-limited queue.

`plan.md` is the source of truth for contracts and behaviour. `checklist.md` is
the dependency-ordered implementation plan; work through it one task at a time.
