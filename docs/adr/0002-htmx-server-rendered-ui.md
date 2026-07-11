# ADR-0002: Server-rendered UI with html/template + HTMX

Date: 2026-07-11
Status: accepted

## Context

v1 needs a web UI (~5 screens: Dashboard/Backups, Connections, Schedules, Destinations, Settings). Options considered: Go templates + HTMX; Go API + Nuxt 3 frontend; hybrid (start HTMX, add Nuxt on traction). The product's risk lives in the dump pipeline, not the dashboard.

## Decision

html/template + HTMX, styled with Tailwind via the standalone CLI (no Node on the server), all assets embedded via embed.FS. The binary stays self-contained.

- Live fragments over HTMX: running-backup status polls with `hx-trigger="every 5s"`, test-connection returns a fragment without reload
- Auth: magic link + sessions, no passwords
- HTTP handlers call an internal service layer; the same layer will back a JSON API later — the UI is a replaceable shell

## Consequences

- No second codebase/deploy unit for v1; UI ships inside the same binary
- A Nuxt (or any SPA) frontend and a public API are additive later, not a rewrite
- We accept plainer UX than a SPA would give; acceptable for a developer audience whose main screen is "green checkmarks and history"
