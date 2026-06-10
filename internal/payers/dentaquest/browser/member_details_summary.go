package browser

import (
	"fmt"
	"log"
	"regexp"
	"strings"

	"insurance-benefit-agent-go/internal/logging"
	"insurance-benefit-agent-go/internal/payers/dentaquest/eligibility"
)

// applyMemberDetailsSummaryFromNetwork overlays data from the member-eligibility,
// enrollment-history, and plan-benefit-summary XHR payloads onto el.
func applyMemberDetailsSummaryFromNetwork(s *Session, el *eligibility.PatientEligibility) bool {
	used := false

	if stored := s.GetPayload("member-eligibility"); stored != nil {
		if p := asStringMap(stored.Payload); p != nil {
			used = true

			el.Patient.FullName = firstNonEmpty(anyStr(p, "memberName"), el.Patient.FullName)
			el.Patient.DateOfBirth = firstNonEmpty(anyStr(p, "memberDateOfBirth"), el.Patient.DateOfBirth)
			el.Patient.MemberID = firstNonEmpty(anyStr(p, "memberId"), el.Patient.MemberID)
			el.Patient.MemberEligibility = firstNonEmpty(anyStr(p, "memberEligibilityStatus"), el.Patient.MemberEligibility)
			el.Patient.IsEligible = interpretEligibilityStatus(anyStr(p, "memberEligibilityStatus"), el.Patient.IsEligible)
			el.Patient.GroupNumber = firstNonEmpty(anyStr(p, "unparsedSubGroupNumber"), el.Patient.GroupNumber)

			if level := anyStr(p, "memberCoverageLevel"); level != "" {
				lower := strings.ToLower(level)
				if strings.Contains(lower, "employee") && !regexp.MustCompile(`spouse|child|dependent`).MatchString(lower) {
					el.Patient.MemberType = "Subscriber"
				} else {
					el.Patient.MemberType = "Dependent"
				}
			}

			el.Plan.PlanName = firstNonEmpty(anyStr(p, "memberPlanName"), el.Plan.PlanName)
			el.Plan.GroupName = firstNonEmpty(anyStr(p, "unparsedParentGroupName"), el.Plan.GroupName)
			el.Plan.PlanDesign = firstNonEmpty(anyStr(p, "productNew"), el.Plan.PlanDesign)

			el.NetworkInfo.Type = firstNonEmpty(anyStr(p, "networkNew"), anyStr(p, "networkName"), el.NetworkInfo.Type)
			el.NetworkInfo.DisplayName = firstNonEmpty(anyStr(p, "networkContractNew"), anyStr(p, "networkName"), el.NetworkInfo.DisplayName)
			if el.NetworkInfo.Type != "" || el.NetworkInfo.DisplayName != "" {
				el.NetworkInfo.Confidence = 1
				el.NetworkInfo.Reason = "Parsed from member-eligibility payload"
			}

			setProvision(el, "Coverage level", anyStr(p, "memberCoverageLevel"))
			setProvision(el, "Coverage type", anyStr(p, "memberCoverageType"))
			setProvision(el, "Network", anyStr(p, "networkName"))
			setProvision(el, "Network contract", anyStr(p, "networkContractNew"))
			setProvision(el, "Practitioner", anyStr(p, "practitionerName"))
			setProvision(el, "Location address", anyStr(p, "locationAddress"))
			setProvision(el, "Parent group name", anyStr(p, "unparsedParentGroupName"))
			setProvision(el, "Parent group number", anyStr(p, "unparsedParentGroupNumber"))
			setProvision(el, "Sub group name", anyStr(p, "unparsedSubGroupName"))
			setProvision(el, "Sub group number", anyStr(p, "unparsedSubGroupNumber"))
			setProvision(el, "Product", anyStr(p, "productNew"))
			setProvision(el, "Product category", anyStr(p, "productCategory"))
		}
	}

	if stored := s.GetPayload("enrollment-history"); stored != nil {
		if current := pickCurrentEnrollmentRecord(asSlice(stored.Payload)); current != nil {
			used = true
			el.Patient.EligibilityEffectiveDate = firstNonEmpty(anyStr(current, "effectiveDate"), el.Patient.EligibilityEffectiveDate)
			el.Patient.EligibilityEndDate = firstNonEmpty(normalizeEndDate(anyStr(current, "terminationDate")), el.Patient.EligibilityEndDate)
			el.Plan.PlanName = firstNonEmpty(anyStr(current, "planName"), el.Plan.PlanName)
			el.Plan.GroupName = firstNonEmpty(anyStr(current, "unparsedParentGroupName"), el.Plan.GroupName)
			el.Plan.PlanDesign = firstNonEmpty(anyStr(current, "productNew"), el.Plan.PlanDesign)

			setProvision(el, "Enrollment status", anyStr(current, "status"))
			setProvision(el, "Enrollment effective date", anyStr(current, "effectiveDate"))
			setProvision(el, "Enrollment termination date", anyStr(current, "terminationDate"))
			setProvision(el, "Parent group name", anyStr(current, "unparsedParentGroupName"))
			setProvision(el, "Parent group number", anyStr(current, "unparsedParentGroupNumber"))
			setProvision(el, "Sub group name", anyStr(current, "unparsedSubGroupName"))
			setProvision(el, "Sub group number", anyStr(current, "unparsedSubGroupNumber"))
			setProvision(el, "Product", anyStr(current, "productNew"))
			setProvision(el, "Product category", anyStr(current, "productCategory"))
		}
	}

	if stored := s.GetPayload("plan-benefit-summary"); stored != nil {
		if p := asStringMap(stored.Payload); p != nil {
			used = true
			el.Plan.PlanName = firstNonEmpty(anyStr(p, "planName"), el.Plan.PlanName)
			setProvision(el, "Benefit summary plan name", anyStr(p, "planName"))
			setProvision(el, "Benefit summary plan ID", anyStr(p, "planId"))
		}
	}

	if used {
		log.Printf("[DentaQuest] applied network summary payloads")
		logging.Info("dentaquest.browser", "dentaquest.member.summary.applied", "applied member summary payloads", map[string]any{
			"hasPlanBenefitSummary": s.GetPayload("plan-benefit-summary") != nil,
			"hasMemberEligibility":  s.GetPayload("member-eligibility") != nil,
			"hasEnrollmentHistory":  s.GetPayload("enrollment-history") != nil,
		})
	}
	return used
}

// ── helpers ───────────────────────────────────────────────────────────────────

func normalizeEndDate(value string) string {
	v := normalizeSpace(value)
	if v == "" || v == "9999-12-31" {
		return ""
	}
	return v
}

func setProvision(el *eligibility.PatientEligibility, key, value string) {
	if v := normalizeSpace(value); v != "" {
		if el.Plan.Provisions == nil {
			el.Plan.Provisions = make(map[string]string)
		}
		el.Plan.Provisions[key] = v
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func interpretEligibilityStatus(value string, current bool) bool {
	v := strings.ToLower(normalizeSpace(value))
	if v == "" {
		return current
	}
	if strings.HasPrefix(v, "active") {
		return true
	}
	if regexp.MustCompile(`inactive|gap|terminated|termination|expired|cancelled|canceled|ineligible`).MatchString(v) {
		return false
	}
	return current
}

func pickCurrentEnrollmentRecord(records []any) map[string]any {
	if len(records) == 0 {
		return nil
	}
	for _, r := range records {
		if m := asStringMap(r); m != nil {
			if strings.ToLower(normalizeSpace(fmt.Sprint(m["status"]))) == "active" {
				return m
			}
		}
	}
	var best map[string]any
	var bestDate string
	for _, r := range records {
		m := asStringMap(r)
		if m == nil {
			continue
		}
		d := normalizeSpace(fmt.Sprint(m["effectiveDate"]))
		if best == nil || d > bestDate {
			best = m
			bestDate = d
		}
	}
	return best
}
