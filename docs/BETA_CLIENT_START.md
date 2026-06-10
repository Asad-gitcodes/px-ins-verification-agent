# Beta Client Start Guide

## 1. Install Folder

Create one folder on the client machine:

```text
C:\PX\Agent
```

Place these files in that folder:

```text
px-ins-agent.exe
agent-updater.exe
browser\
  chrome.exe
```

Extract `browser.zip` into this folder so `browser\chrome.exe` and its supporting Chromium files sit next to `px-ins-agent.exe`. This prevents first-run Chromium downloads on client firewalls.

No `agent.config.json` is required for production or beta installs.

## 2. Start Agent

Open PowerShell:

```powershell
cd C:\PX\Agent
.\px-ins-agent.exe `
  --office-key OFFICE_KEY_FROM_PATCON `
  --patcon-url https://patcon.8px.us `
  --patcon-token PATCON_TOKEN `
  --port 8080 `
  --api-token LOCAL_AGENT_API_TOKEN
```

Leave this window open during beta testing. For a service install, use the same flags in the service command line.

## 3. Verify Agent

Check binary version:

```powershell
.\px-ins-agent.exe --version
```

Check running API:

```powershell
$token = "LOCAL_AGENT_API_TOKEN"
Invoke-RestMethod -Method GET `
  -Uri "http://127.0.0.1:8080/api/v1/version" `
  -Headers @{ Authorization = "Bearer $token" }
```

## 4. Trigger Test Run

Run one patient:

```powershell
$token = "LOCAL_AGENT_API_TOKEN"
Invoke-RestMethod -Method POST `
  -Uri "http://127.0.0.1:8080/api/v1/triggers" `
  -Headers @{ Authorization = "Bearer $token" } `
  -ContentType "application/json" `
  -Body '{"action":"run_now","patnum":"1234","requestedBy":"beta-test"}'
```

Run all eligible appointments for today:

```powershell
Invoke-RestMethod -Method POST `
  -Uri "http://127.0.0.1:8080/api/v1/triggers" `
  -Headers @{ Authorization = "Bearer $token" } `
  -ContentType "application/json" `
  -Body '{"action":"run_all","addDays":0,"requestedBy":"beta-test"}'
```

## 5. Logs And Outputs

Appointment field write-back:

```text
Open Dental apptfield: HRDView
Active primary coverage: V1
Non-active/failure outcomes: NV1: {reason}
```

Current `NV1` reasons are:

```text
NV1: Coverage found but inactive
NV1: Patient/member not found
NV1: Invalid/missing member info
NV1: Payer/system failure
```

Patient/member data problems are finalized for office correction. Payer, site,
or system failures are retried first and only finalized after retries are
exhausted.

Logs:

```text
C:\PX\Agent\logs
```

Optional debug artifacts, when enabled:

```text
C:\PX\Agent\artifacts
```

Local PDFs, when `localPdfOnly` is enabled:

```text
C:\PX\Agent\pdfs
```

## 6. Update Test

Production update source is PatCon. The agent reads update defaults from the embedded `internal/config/defaults.json` and uses the startup PatCon URL/token.

Check update:

```powershell
Invoke-RestMethod -Method GET `
  -Uri "http://127.0.0.1:8080/api/v1/update/check" `
  -Headers @{ Authorization = "Bearer $token" }
```

Apply update:

```powershell
Invoke-RestMethod -Method POST `
  -Uri "http://127.0.0.1:8080/api/v1/update/apply" `
  -Headers @{ Authorization = "Bearer $token" }
```

The updater replaces `px-ins-agent.exe`, creates `px-ins-agent.exe.bak`, and restarts the agent with the same arguments.

If restart fails, check:

```text
C:\PX\Agent\agent-updater.log
```
