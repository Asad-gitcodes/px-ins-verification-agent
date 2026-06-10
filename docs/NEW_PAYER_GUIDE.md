# Adding a New Payer — Step-by-Step

The `internal/payers/newpayer/` folder is a fully-wired template.
Follow these steps to onboard any new insurance portal in ~1–2 hours of work.

---

## Step 1 — Copy and rename

```bash
cp -r internal/payers/newpayer internal/payers/YOURPAYER
```

Then do a bulk rename in the new folder:

```bash
cd internal/payers/YOURPAYER
# Replace package names and identifiers:
sed -i '' 's/newpayer/YOURPAYER/g' adapter.go browser/session.go browser/mfa.go eligibility/builder.go
sed -i '' 's/NewPayer/YourPayer/g' adapter.go browser/session.go browser/mfa.go eligibility/builder.go
sed -i '' 's/NEWPAYER/YOURPAYER/g' adapter.go browser/session.go browser/mfa.go eligibility/builder.go
```

---

## Step 2 — Set the payer URL constant

In `adapter.go`, set `PayerURL` to the exact string PatCon sends in `payer.PayerURL`.
Check an existing job log or the PatCon API response. Examples:

```
"DentaQuest.com"
"metlife.com"
"DeltaDentalIns.com"
"Guardian.com"
```

---

## Step 3 — Fill in the login URL

In `browser/session.go` set `loginURL` and `homeURL` to the portal's real URLs.

---

## Step 4 — Fill in the credential form selectors

Open the portal in Chrome → DevTools → Elements panel.
Find the username field, password field, and submit button.
Right-click each → **Copy** → **Copy selector**.

Paste those into `fillCredentials()` in `browser/session.go`:

```go
usernameEl, err := page.Timeout(20*time.Second).Element(`#your-real-selector`)
passwordEl, err  := page.Timeout(10*time.Second).Element(`#your-real-selector`)
submitEl, err    := page.Timeout(10*time.Second).Element(`#your-real-selector`)
```

---

## Step 5 — Fill in the "logged-in" detector

`isLoggedIn()` must return `true` only when the session is authenticated.
Pick any element that only appears on the authenticated portal (nav bar, eligibility search form, etc.):

```go
_, err := page.Timeout(2*time.Second).Element(`#portal-nav-bar`)
return err == nil
```

---

## Step 6 — Implement ProbePatient

`ProbePatient()` in `browser/session.go` is the core scraping function.
It must:

1. Navigate to the eligibility / member search page.
2. Enter subscriber ID / DOB / name from the `appt` argument.
3. Submit the search form and wait for results.
4. Detect "not found" → return `(nil, nil)`.
5. Detect errors → return `(nil, err)`.
6. Scrape all benefit/plan data from the detail page.
7. Return it as `*RawProbeData` (cast to `any`).

Expand `RawProbeData` in `adapter.go` to hold all the fields you scrape:

```go
type RawProbeData struct {
    MemberID         string
    MemberName       string
    DOB              string
    EligibilityStatus string
    PlanName         string
    CarrierName      string
    GroupName        string
    GroupNumber      string
    EffectiveDate    string
    TermDate         string
    PreventivePct    float64
    BasicPct         float64
    MajorPct         float64
    DeductibleLimit  float64
    DeductibleUsed   float64
    AnnualMaxLimit   float64
    AnnualMaxUsed    float64
}
```

---

## Step 7 — Fill in the eligibility builder

In `eligibility/builder.go`, map each `RawProbeData` field to the canonical
`eligibility.PatientEligibility` struct.  The comments in the file show exactly
which fields to set.  This is mostly a one-to-one copy.

---

## Step 8 — Configure MFA (if required)

Edit `browser/mfa.go`:

- **No MFA**: set `MFAMethod = "none"` in the credential config and leave the file as-is.
- **SMS**: update `otpPattern` if the code isn't 6 digits, update the OTP input selector and submit button selector.
- **Email**: same as SMS but call `completeEmailMFA`.
- **Authenticator app (TOTP)**: the code is already in the office credential record as `input.Credential.TOTPSecret`; use `mfa.GetTOTPCode(secret)`.

---

## Step 9 — Register the adapter

In `internal/app/app.go`, add one import and one register call:

```go
import (
    ...
    "insurance-benefit-agent-go/internal/payers/YOURPAYER"
)

// inside New():
registry.Register(YOURPAYER.NewAdapter())
```

---

## Step 10 — Build and test

```bash
go build ./...
```

For a local test run with a real credential:

```bash
# Set testing.writePdf = true and headless = false in agent.local.json
# then run the agent with --run-once
./dist-local/agent --run-once
```

Watch the logs for `[YourPayer] probe written` per patient and
`[YourPayer] result … status=Verified` in phase 2.

---

## What's automatic (you don't touch)

| Thing | Where it lives |
|---|---|
| PDF generation | `internal/pdf/` |
| OpenDental write-back | `internal/odetrans/` |
| Appointment field updates | `internal/resultwriter/` |
| Job queuing / polling | `internal/jobmgr/` |
| Auth cookie caching | `internal/browser/session.go` |
| Benefit % maths | `internal/advanced/` |
| Logging / events | `internal/logging/` |

All of the above run automatically once `eligibility/builder.go` returns
a populated `*eligibility.PatientEligibility`.
