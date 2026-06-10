# PatientX Insurance Benefit Agent - Overview

## What It Does

The Insurance Benefit Agent verifies dental insurance eligibility for upcoming OpenDental appointments. Instead of staff manually logging into payer portals, the agent receives appointment work, checks eligibility through payer-specific adapters, and writes results back to the practice workflow.

## The Problem It Solves

Dental offices see patients across many payer portals and clearinghouse-style tools. Before each appointment, staff often need to:

1. Log into the payer or clearinghouse portal
2. Search for the patient and subscriber
3. Confirm active, inactive, or not-found status
4. Review maximums, deductibles, coverage percentages, and history
5. Copy the useful information into OpenDental

This is slow, inconsistent, and easy to miss when a schedule has many appointments across multiple carriers.

## How It Works

The agent runs on a Windows computer at the dental office. Work is delivered through its local API by the PatientX platform or a direct trigger.

### 1. Receive Work

The agent supports:

- **Single-patient runs** - verify one appointment immediately.
- **Run-all / day runs** - verify all eligible appointments for a date or appointment range.
- **Payload-based runs** - use appointments, provider identity, and practice identity supplied by the trigger.
- **Query-backed runs** - when payload data is missing, query OpenDental through PatCon for appointments, provider, and practice details.

Appointments are grouped by payer and processed by the matching adapter.

### 2. Verify Eligibility

Each payer adapter handles its own login, search, API probing, browser capture, and parsing. The normalized output is an advanced eligibility JSON containing:

- Active, inactive, not-found, or patient-error status
- Payer, plan, group, subscriber, and patient details
- Annual and lifetime maximums, including remaining amounts
- Individual and family deductibles
- Coverage matrix by category and network tier
- Treatment-plan code coverage where available
- Treatment history, frequency limits, age limits, and plan notes where available
- Source/provenance, including whether the result came from a payer API, clearinghouse page, or stub/error fallback

### 3. Write Results Back

For each patient, the agent can produce:

- **PDF eligibility report** attached to the OpenDental patient record
- **Appointment field status** such as `Verified`, `Inactive`, `Not Found`, or an error outcome
- **Advanced JSON artifacts** for audit/debugging
- **Synthetic 270/271 EDI artifacts** for OpenDental eTrans compatibility and manual validation
- **SQL insert preview** for `etrans` and `etransmessagetext` review before enabling database insertion

The PDF and appointment-field write-backs can be enabled or disabled by server config and local testing settings.

## Supported Payer Adapters

These adapters are currently registered in the agent:

| Adapter | Common Payer URL / Alias | Method |
|---|---|---|
| Delta Dental Insurance | `deltadentalins.com` | Portal login plus payer API capture |
| DentalXChange ClaimConnect | `DentalXchange.com`, `claimconnect` | Browser session plus ClaimConnect/API response parsing |
| DentaQuest | `DentaQuest.com` | Portal login plus captured payer APIs |
| Denti-Cal | `dentical.com`, `denti-cal.com` | Portal/API adapter |
| EmblemHealth | `emblemhealth.com` | Portal automation plus Apex/API capture |
| Guardian | `GuardianLife.com`, `guardiananytime.com` | Guardian Anytime search/API capture |
| MetLife Dental | `metlife.com` | Portal login plus payer API capture |
| UnitedHealthcare Dental | `uhcdental.com` | Portal login plus payer API capture |
| Vyne Trellis | `vynetrellis.com` | Trellis API adapter |

DentalXChange, Denti-Cal, and Vyne/Trellis are included. The repo also has stub/provenance handling for supported payer aliases so not-found, inactive, unsupported, and parse-error cases still produce useful minimum output.

## OD eTrans / EDI Output

The agent can synthesize OpenDental-compatible eligibility EDI artifacts for verified or inactive results:

- `*_270Request.edi`
- `*_271Response.edi`
- `*_etrans_insert.sql`

The generated 271 includes:

- `EB*1**30` active coverage or `EB*6**30` inactive coverage
- OD-readable dental EB03 categories such as Diagnostic, X-Ray, Preventive, Restorative, Endo, Perio, Crowns, Ortho, Prosthodontics, Oral Surgery, Adjunctive, Maxillofacial Prosthetics, Accident, Diagnostic Lab, and Anesthesia
- Coinsurance percentages in EDI decimal form, for example `0.2` for patient pays 20%
- Remaining maximum rows first, for example `EB*F*IND*35***29*...`
- Calendar/lifetime total maximum rows where available
- Individual/family deductible rows where available

The current implementation writes EDI and SQL preview files for validation. Database insertion into OpenDental eTrans can be enabled later once the generated request/response pairs are confirmed in OD.

## Key Features

**API-driven** - The agent runs when triggered. It does not depend on an internal schedule.

**Carrier-specific adapters** - Each payer has isolated code for login, probing, parsing, and retry behavior.

**Probe-first debugging** - Raw payer/API responses can be written to `artifacts/_probe_bucket` so parsing can be replayed and improved without repeatedly hitting payer portals.

**Crash recovery and queueing** - Runs can be queued and resumed so interrupted work does not require starting over manually.

**Retry and patient-level errors** - A single patient failure does not block the payer run. Outcomes are tracked as verified, inactive, not found, or patient error.

**OpenDental integration** - Appointment queries include patient, subscriber, carrier, payer ID, ordinal, `InsSubNum`, and `PlanNum` so results can be tied back to the correct primary or secondary insurance plan.

**Provider/practice identity resolution** - Triggers can provide provider and practice payloads. If not supplied, the agent can query OpenDental preferences/provider rows through PatCon and use those values in synthetic EDI.

**Audit artifacts** - PDFs, eligibility JSON, advanced JSON, probe JSON, EDI files, and SQL previews make it possible to inspect what happened after a run.

## MFA Addition Process

The agent supports payer MFA through shared mailbox-based email MFA and payer-specific SMS MFA helpers. For new email MFA payer credentials, use the shared plus-address convention:

```text
mfains+{accountId}+{carrier}@hrdsq.com
```

Examples:

```text
mfains+976+dentaquest@hrdsq.com
mfains+976+metlife@hrdsq.com
mfains+976+deltadental@hrdsq.com
```

### Server/Credential Setup

For a payer that uses email MFA:

1. Set the payer credential `mfaMethod` to `email`.
2. Set the payer portal username to either:
   - Full plus alias style: `mfains+976+dentaquest`
   - Compact style: `mfains976dentaquest`
   - Full email address if the portal requires it: `custom@example.com`
3. Configure the shared MFA mailbox in scraper config:
   - `mfa.email.host`
   - `mfa.email.port`
   - `mfa.email.secure`
   - `mfa.email.user`, usually `mfains@hrdsq.com`
   - `mfa.email.password`
   - optional `mfa.email.mailbox`, `timeoutMs`, and `pollIntervalMs`
4. Enroll the payer portal MFA email destination using the plus alias for that account and carrier.

### How The Agent Uses It

At runtime, `jobmgr.buildPayerSessionInput` builds an `mfa.EmailConfig` only when the selected credential has:

```text
mfaMethod = email
```

The expected recipient is derived by `expectedMFAToAddress(mailboxUser, credential.Username)`:

- `mfains+976+dentaquest` becomes `mfains+976+dentaquest@hrdsq.com`
- `mfains976dentaquest` also becomes `mfains+976+dentaquest@hrdsq.com`
- `custom@example.com` stays `custom@example.com`

The MFA reader then polls the configured IMAP mailbox and filters messages by:

- message date after the MFA request time, with a small freshness skew
- matching `To:` recipient when `ExpectedTo` is available
- first six-digit code in subject/body, including “enter a code instead” style messages

After a code is found, matching verification emails are moved to the cleanup mailbox, currently `[Gmail]/Trash`, when `DeleteAfterRead` is enabled.

### Payer Support

Email MFA is currently used by payer flows that call `mfa.GetEmailCode`, including DentaQuest, Delta Dental, EmblemHealth, and MetLife where email MFA is selected. Some payers also support SMS MFA through payer-specific handlers and the shared SMS helper.

### MFA Troubleshooting

- If no code is found, confirm the portal sends to the exact plus alias the agent expects.
- If messages are skipped as “different recipient,” compare the email `To:` header with `ExpectedTo`.
- If messages are skipped as old, check the local machine clock and the MFA request timestamp.
- If mailbox login fails, check `MFAUSER`, `MFAPASS`, host, port, and secure mode.
- For local testing, `agent.local.json` can override the MFA mailbox password via `mfaPassword`.

## What Staff See in OpenDental

After a successful run, staff can see:

- Appointment verification status on the schedule
- A PDF eligibility report attached to the patient
- Minimum payer/source/reason information even for inactive, not-found, unsupported, or unable-to-determine cases

When eTrans insertion is enabled, OD can also show the synthetic 270 request and 271 response in the electronic benefit request UI, allowing benefit rows to be imported from the generated 271.

## Deployment

The agent runs as a background Windows process at the office. It is started with required flags for:

- Office key
- PatCon URL
- PatCon token
- Local API port

Optional config controls sweep behavior, PDF behavior, update behavior, and local testing options such as local PDF only, all appointments, max appointments, headless mode, debug artifacts, and appointment-field write-back.

Carrier credentials and payer snapshots are managed centrally by the PatientX platform and delivered to the agent at runtime.

## Things To Watch

- Payer portals change frequently; probe artifacts are important for fast parser updates.
- OD eTrans insertion should stay behind validation until generated 270/271 rows are confirmed in OD for several payer styles.
- Some payer maximums are shared across categories. The synthetic 271 expands these when the source clearly says "Shared with".
- Percentages in EB segments must use EDI decimal format, not whole percentages.
- Dates in `etrans.DateTimeTrans`, 270/271 envelopes, and service-date segments should be aligned when manually inserting rows.
