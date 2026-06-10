# Agent Update System

## Overview

The agent supports self-updating without manual file replacement. When an update is available, the agent downloads the new binary, verifies its SHA256, and uses `agent-updater.exe` to hot-swap itself.

Update sources:

- `patcon`: production source. Fetches manifest and binary from the PatCon server.
- `local`: test source. Reads manifest and binary from a local `updates/` folder next to the exe.

## Runtime Flow

In API mode, the agent also runs a background update checker using
`updates.checkIntervalMinutes`. On each interval it skips the check if a scrape
run is active; when idle, it checks the manifest and applies an available update.

### Check: `GET /api/v1/update/check`

1. Reads `manifest.json` from PatCon or local disk.
2. Compares `manifest.version` against the running version.
3. Checks `manifest.channel` against the configured channel.
4. For local source, verifies the staged binary SHA256 immediately.
5. For PatCon source, reports availability; binary verification happens after download in apply.

### Apply: `POST /api/v1/update/apply`

1. Calls check internally and aborts if no update is available.
2. For PatCon source, downloads `px-ins-agent.exe` from PatCon.
3. Verifies the downloaded binary SHA256.
4. Launches `agent-updater.exe`.
5. The updater waits for the agent to exit, backs up `px-ins-agent.exe`, replaces it, and restarts the agent with the same arguments.

## Release Flow

```text
Developer pushes git tag, for example v1.2.0
GitHub Actions builds release assets
GitHub Release receives:
  px-ins-agent.exe
  agent-updater.exe
  px-ins-agent.exe.sha256
  manifest.json
PatCon ingests the release assets
PatCon serves:
  GET /updates/manifest.json
  GET /updates/px-ins-agent.exe
Agent checks and applies updates through its local API
```

## manifest.json

Published with every GitHub release:

```json
{
  "version": "1.2.0",
  "commit": "a1b2c3d",
  "builtAt": "2026-04-29T10:00:00Z",
  "channel": "stable",
  "asset": "px-ins-agent.exe",
  "updaterAsset": "agent-updater.exe",
  "sha256": "abc123..."
}
```

| Field | Purpose |
|---|---|
| `version` | Semver version compared against running agent. |
| `channel` | `stable`, `rc`, or `beta`. |
| `asset` | Binary filename to download. |
| `updaterAsset` | Name of updater binary shipped with the release. |
| `sha256` | SHA256 of `asset`; verified before replacement. |

No config template is shipped. Production installs use embedded defaults plus startup flags.

## Configuration

Production update defaults live in:

```text
internal/config/defaults.json
```

Example:

```json
{
  "updates": {
    "enabled": true,
    "source": "patcon",
    "channel": "stable",
    "checkIntervalMinutes": 60
  }
}
```

PatCon URL and token come from the agent startup flags:

```text
--patcon-url
--patcon-token
```

No per-office update config file is required.

### Channel Rules

| Office channel | Receives |
|---|---|
| `stable` | Stable releases only. |
| `rc` | RC and stable releases. |
| `beta` | Beta, RC, and stable releases. |

## Manual Update

Run these on the office machine:

```powershell
$token = "LOCAL_AGENT_API_TOKEN"
Invoke-RestMethod -Method GET `
  -Uri "http://127.0.0.1:8080/api/v1/update/check" `
  -Headers @{ Authorization = "Bearer $token" }
```

```powershell
Invoke-RestMethod -Method POST `
  -Uri "http://127.0.0.1:8080/api/v1/update/apply" `
  -Headers @{ Authorization = "Bearer $token" }
```

## Local Testing

For local update testing, use an optional local config passed with `--config`:

```json
{
  "updates": {
    "enabled": true,
    "source": "local",
    "localDir": "updates",
    "channel": "beta"
  }
}
```

Place files next to `px-ins-agent.exe`:

```text
px-ins-agent.exe
agent-updater.exe
browser/
  chrome.exe
updates/
  manifest.json
  px-ins-agent.exe
```

## PatCon Requirements

1. Ingest GitHub release assets when a release is published.
2. Store assets by channel, for example `stable`, `beta`, or `rc`.
3. Serve authenticated endpoints:

```text
GET /updates/manifest.json
GET /updates/px-ins-agent.exe
```

4. Optionally choose channel per office server-side.

## Important Notes

- `agent-updater.exe` must be present next to `px-ins-agent.exe`.
- The packaged Chromium folder should be present as `browser\chrome.exe` plus its supporting files. The agent prefers that binary and will not download Chromium at runtime when it is present.
- The updater restarts the agent with the same command-line arguments.
- On update failure, `px-ins-agent.exe.bak` remains on disk for manual rollback.
- Runtime scraper config, payer credentials, and work snapshots are memory-only and are not persisted as part of the update system.
