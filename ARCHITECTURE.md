# Insurance Benefit Agent Architecture

## Overview

The agent is a Go process installed on-premise at a dental office. It exposes a loopback REST API, receives work from PatCon or an authorized local caller, verifies insurance eligibility through payer portals/APIs, generates optional PDF benefit summaries, and writes results back through PatCon/OpenDental integration points.

The agent is API-driven. It does not own the production schedule; PatCon decides when work should run and calls the local API.

The agent is intentionally stateless across restarts. Runtime scraper config, payer credentials, office-code data, and work snapshots are held in memory only. If PatCon is unavailable, the agent remains idle and waits for future triggers.

## Components

### `cmd/agent`

Main entrypoint. Parses required startup flags, loads embedded defaults, applies CLI runtime values, configures logging, constructs `App`, and starts the local API.

Required flags:

- `--office-key`
- `--patcon-url`
- `--patcon-token`
- `--port`

Optional flags:

- `--api-token`
- `--config`
- `--run-once`
- `--version`

### `internal/config`

Loads embedded defaults from `internal/config/defaults.json`. Optional local/test config can be supplied with `--config`, but production does not require a config file.

`agent.local.json` is supported for local support overrides only and must not be committed.

### `internal/triggerapi`

Local HTTP API server bound to `127.0.0.1:<port>`.

Auth:

```text
Authorization: Bearer <api-token>
```

or:

```text
X-Agent-Trigger-Token: <api-token>
```

Endpoints:

| Method | Path | Auth | Description |
|---|---|---|---|
| `GET` | `/healthz` | No | Health check. |
| `GET` | `/api/v1/status` | Yes | Current run state. |
| `GET` | `/api/v1/version` | Yes | Binary version metadata. |
| `POST` | `/api/v1/triggers` | Yes | Dispatch work. |
| `GET` | `/api/v1/update/check` | Yes | Check for update. |
| `POST` | `/api/v1/update/apply` | Yes | Apply update. |

Trigger examples:

```json
{ "action": "run_now", "patnum": "1235", "requestedBy": "patcon" }
```

```json
{ "action": "run_all", "addDays": 1, "requestedBy": "scheduler" }
```

### `internal/controlplane`

PatCon API client.

- `FetchServerConfig`: gets current office scraper config, payer credentials, MFA config, and office settings.
- `StartPayerTracking`: opens a payer tracking record.
- `EndPayerTracking`: closes the tracking record with result details.

### `internal/jobmgr`

Owns run state, queue files, trigger deduplication, and payer dispatch.

Behavior:

- One active run at a time.
- Duplicate running or queued requests return the existing run ID.
- If a run is active, new non-duplicate triggers are persisted to the local queue.
- Each run fetches fresh PatCon data before processing.

### `internal/cache`

In-memory work snapshot only. The snapshot exists to share one freshly fetched PatCon view across a run and to support in-process retry behavior. It is not persisted to disk.

### `internal/officecodes`

Fetches office procedure codes from PatCon and keeps an in-memory cache for the current process. No disk cache is written.

### `internal/payers`

Payer adapter registry and common contracts. Each payer implements the `Adapter` interface.

Registered adapters include:

- DentalXChange
- DentaQuest
- Delta Dental
- MetLife
- UHC Dental
- Vyne Trellis

### `internal/appointments`

Selects appointments from PatCon/OpenDental-backed data for either a single patient or a date offset.

### `internal/pdf`

Renders eligibility summaries to PDF using headless Chrome via rod.

### `internal/resultwriter`

Writes eligibility status and PDF results back through PatCon/OpenDental integration points.

### `internal/updater`

Checks update manifests, downloads new binaries, verifies SHA256, and launches `agent-updater.exe` to hot-swap the running binary.

## Data Flow

```text
PatCon / authorized caller
  POST /api/v1/triggers
        |
        v
triggerapi
        |
        v
jobmgr
        |
        +--> controlplane.FetchServerConfig
        |
        +--> officecodes.GetOfficeCodes
        |
        +--> appointments.Select
        |
        +--> controlplane.StartPayerTracking
        |
        +--> payer Adapter.Run
        |       +--> browser/API payer interactions
        |       +--> eligibility summary
        |       +--> optional PDF generation
        |
        +--> resultwriter.ApplyResult
        |
        +--> controlplane.EndPayerTracking
```

## Configuration Model

Production:

```text
embedded defaults + startup flags
```

Embedded file:

```text
internal/config/defaults.json
```

Startup example:

```powershell
.\px-ins-agent.exe `
  --office-key OFFICE_KEY_FROM_PATCON `
  --patcon-url https://patcon.8px.us `
  --patcon-token PATCON_TOKEN `
  --port 8080 `
  --api-token LOCAL_AGENT_API_TOKEN
```

No production `agent.config.json` is required.

## State And Security

The following are memory-only:

- scraper config
- payer credentials
- MFA config
- office-code data
- work snapshots

The following may be written locally:

- logs under `logs/`
- queue files under `queue/`
- optional debug artifacts under `artifacts/`
- optional local PDFs under `pdfs/`
- updater backup/log files

Do not commit local configs, snapshots, browser profiles, logs, artifacts, or generated binaries.
