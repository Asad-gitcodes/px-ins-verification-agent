# Insurance Benefit Agent Go

Go-based office agent for automated dental insurance eligibility verification.

## Current Model

The agent is installed on a Windows machine at a dental office. On startup it exposes a local HTTP API on `127.0.0.1:<port>` and waits for PatCon, or an authorized local caller, to trigger work.

The agent is intentionally stateless across restarts. Runtime scraper config, payer credentials, office-code data, and work snapshots are held in memory only. If PatCon is unavailable, the agent remains idle and waits for future API triggers rather than running from persisted cached work.

## Package Layout

- `cmd/agent`: main process entrypoint
- `cmd/agent-updater`: helper process used for hot-swap binary updates
- `cmd/dqapiprobe`: DentaQuest API probe utility
- `internal/app`: application wiring
- `internal/config`: embedded defaults, optional local overrides, and CLI-applied runtime settings
- `internal/jobmgr`: trigger handling and payer run orchestration
- `internal/controlplane`: PatCon API client
- `internal/triggerapi`: local authenticated API server
- `internal/payers`: payer adapter contract and registry
- `internal/pdf`: PDF writer boundary
- `internal/resultwriter`: OpenDental/status/PDF write-back
- `internal/logging`: structured event logging
- `internal/models`: shared DTOs/contracts
- `internal/mfa`: email/SMS MFA helpers
- `docs`: deployment, update, and migration notes

## Build

```powershell
go test ./...
go build -o .\dist-local\px-ins-agent.exe .\cmd\agent
go build -o .\dist-local\agent-updater.exe .\cmd\agent-updater
```

## Run

Required runtime values are passed as flags. A per-office `agent.config.json` is no longer required.

```powershell
.\px-ins-agent.exe `
  --office-key OFFICE_KEY_FROM_PATCON `
  --patcon-url https://patcon.8px.us `
  --patcon-token PATCON_TOKEN `
  --port 8080 `
  --api-token LOCAL_AGENT_API_TOKEN
```

`--api-token` is optional. If omitted, the agent uses `--patcon-token` as the local API bearer token.

## Local API

Default base URL:

```text
http://127.0.0.1:8080
```

Authenticated endpoints accept:

```text
Authorization: Bearer <api-token>
```

Primary endpoints:

- `GET /healthz`
- `GET /api/v1/status`
- `GET /api/v1/version`
- `POST /api/v1/triggers`
- `GET /api/v1/update/check`
- `POST /api/v1/update/apply`

## Configuration

Defaults are embedded from `internal/config/defaults.json` at build time. Only non-sensitive defaults belong in the repository.

Optional local developer overrides can be supplied with `agent.local.json`, but that file is not required for production and must not be committed.

## Release Assets

GitHub releases publish:

- `px-ins-agent.exe`
- `agent-updater.exe`
- `px-ins-agent.exe.sha256`
- `manifest.json`
- `browser.zip`

PatCon ingests those assets and serves the manifest/binary to installed agents through the update endpoints.

`browser.zip` contains the Chromium runtime used by rod. Extract it next to `px-ins-agent.exe` so the installed layout contains `browser\chrome.exe`; the agent will use that packaged binary before it tries any system browser or rod-managed download.
