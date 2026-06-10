# DentaQuest Migration Plan

## Source Mapping

The initial Go repo is based on the current Node implementation under `src/payers/dentaQuest`.

Primary source files mapped so far:
- `src/payers/dentaQuest/index.js`
- `src/payers/dentaQuest/oktaFlow.js`
- `src/payers/dentaQuest/loginFlow.js`
- `src/payers/dentaQuest/portalFlow.js`
- `src/payers/dentaQuest/providerSelectionFlow.js`
- `src/payers/dentaQuest/memberSearchFlow.js`
- `src/payers/dentaQuest/memberDetailsFlow.js`
- `src/payers/dentaQuest/memberDetailNetworkFlow.js`
- `src/payers/dentaQuest/memberDetailsSummaryFlow.js`
- `src/payers/dentaQuest/maximumDeductibleNetworkFlow.js`
- `src/payers/dentaQuest/accumulatorsFlow.js`
- `src/payers/dentaQuest/treatmentHistoryFlow.js`
- `src/payers/dentaQuest/buildPatientEligibility.js`

## Proposed Go Package Split

- `internal/payers/dentaquest/adapter.go`
  Owns the session pipeline currently centered in `index.js`.
- `internal/payers/dentaquest/browser`
  Owns login, provider selection, member search, and page transitions.
- `internal/payers/dentaquest/api`
  Owns Max API and any response normalization that does not need browser state.
- `internal/models`
  Holds shared DTOs that replace ad hoc JS objects.

## Recommended Port Order

1. Control-plane registration and encrypted secret handling.
2. Browser session bootstrap with Playwright-Go.
3. Portal login and Okta/email MFA.
4. Provider selection.
5. Member search batching and opening member details.
6. Max API-backed extraction from member details onward.
7. Eligibility payload shaping and parity validation.

## Notes

- DentaQuest is the best first payer because much of the downstream value appears API-driven.
- Keep the Node agent available during migration so results can be compared side by side.
- Avoid porting all payers before DentaQuest reaches parity.
