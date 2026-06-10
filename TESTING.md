# Test Run Guide

Production installs use embedded defaults plus startup flags. For local testing, you can pass an optional JSON file with `--config` to override non-required settings such as `testing`.

Example startup:

```powershell
.\px-ins-agent.exe `
  --office-key OFFICE_KEY_FROM_PATCON `
  --patcon-url https://patcon.8px.us `
  --patcon-token PATCON_TOKEN `
  --port 8080 `
  --api-token LOCAL_TEST_TOKEN `
  --config .\agent.test.json
```

Do not commit local test config files.

## Test Mode Flags

Example `agent.test.json`:

```json
{
  "testing": {
    "skipTracking": true,
    "skipApptField": true,
    "localPdfOnly": true,
    "allAppointments": true,
    "maxAppointments": 3,
    "writePdf": true
  }
}
```

| Flag | Type | Default | What it does |
|---|---|---|---|
| `skipTracking` | bool | false | Skips `StartPayerTracking` and `EndPayerTracking` API calls to PatCon. |
| `skipApptField` | bool | false | Skips writing `VerifyInsurance` status to the OD appointment field. |
| `localPdfOnly` | bool | false | Writes PDFs to `./pdfs/` instead of uploading to PatCon. |
| `allAppointments` | bool | false | Removes the appointment-field filter from the appointment query. |
| `maxAppointments` | int | 0 | Caps appointments processed per payer. `0` means no cap. |
| `apptRangeDays` | int | server value | Overrides the appointment date range. |
| `writePdf` | bool | server value | Explicitly force PDF generation on or off. |
| `updateApptField` | bool | true | Legacy field; prefer `skipApptField`. |
| `writeDebugArtifacts` | bool | false | Saves debug artifacts under `artifacts/`. |

## Recommended Configurations

Full local test, with no production writes:

```json
{
  "testing": {
    "skipTracking": true,
    "skipApptField": true,
    "localPdfOnly": true,
    "allAppointments": true,
    "maxAppointments": 3,
    "writePdf": true
  }
}
```

Beta run, with tracking and PDF upload live but no appointment stamp:

```json
{
  "testing": {
    "skipApptField": true
  }
}
```

Production:

```text
No testing config.
```

## Triggering A Test

Once the agent is running:

```powershell
$token = "LOCAL_TEST_TOKEN"
Invoke-RestMethod -Method POST `
  -Uri "http://127.0.0.1:8080/api/v1/triggers" `
  -Headers @{ Authorization = "Bearer $token" } `
  -ContentType "application/json" `
  -Body '{"action":"run_now","patnum":"1234","requestedBy":"local-test"}'
```

Run all appointments for a date offset:

```powershell
Invoke-RestMethod -Method POST `
  -Uri "http://127.0.0.1:8080/api/v1/triggers" `
  -Headers @{ Authorization = "Bearer $token" } `
  -ContentType "application/json" `
  -Body '{"action":"run_all","addDays":1,"requestedBy":"local-test"}'
```

## Local Outputs

When `localPdfOnly` is enabled, PDFs are written under:

```text
pdfs/
```

When `writeDebugArtifacts` is enabled, diagnostic files are written under:

```text
artifacts/
```
