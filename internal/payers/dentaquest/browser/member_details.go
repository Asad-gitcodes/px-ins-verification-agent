package browser

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/logging"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers/dentaquest/eligibility"

	"github.com/go-rod/rod"
)

// IsMemberDetailsPage returns true when the current page is a member-details page.
func IsMemberDetailsPage(page *rod.Page) bool {
	if regexp.MustCompile(`(?i)/member-details/`).MatchString(currentURL(page)) {
		return true
	}
	return isVisible(page, `[data-testid="member-information-header"], h1`)
}

// ScrapeMemberDetails extracts all eligibility data for the member currently
// shown on the member-details page using only the captured XHR payloads.
func (s *Session) ScrapeMemberDetails(appointment models.Appointment) (*eligibility.PatientEligibility, error) {
	el := buildPatientEligibility(appointment)
	return s.finalizeScrapedMemberDetails(el, appointment.PatNum)
}

// ScrapeCurrentMemberDetails extracts eligibility data using only the
// member-details payloads, without seeding from a requested appointment.
func (s *Session) ScrapeCurrentMemberDetails() (*eligibility.PatientEligibility, error) {
	el := buildEmptyPatientEligibility()
	return s.finalizeScrapedMemberDetails(el, "")
}

func (s *Session) finalizeScrapedMemberDetails(el *eligibility.PatientEligibility, sourcePatNum string) (*eligibility.PatientEligibility, error) {
	applyMemberDetailsSummaryFromNetwork(s, el)
	applyPlanBenefitSummaryNetworkMatrix(s, el)

	el.TreatmentHistory = s.scrapeTreatmentHistoryFromNetwork(100)
	if el.TreatmentHistory == nil {
		el.TreatmentHistory = make(map[string][]eligibility.TreatmentHistoryEntry)
	}

	maxResult := applyMaximumDeductiblePayload(s, el)
	log.Printf(
		"[DentaQuest] maximum-deductible payload: accumulators=%d officeNotes=%d",
		maxResult.ParsedAccumulatorCount, maxResult.AddedOfficeNotes,
	)
	logging.Info("dentaquest.browser", "dentaquest.member.maximum_deductible.parsed", "parsed maximum deductible payload", map[string]any{
		"accumulators": maxResult.ParsedAccumulatorCount,
		"officeNotes":  maxResult.AddedOfficeNotes,
	})

	el.Metadata = eligibility.Metadata{
		EligibilityCheckedAt: time.Now().UTC().Format(time.RFC3339),
		Source:               "WebPortal",
	}

	log.Printf(
		"[DentaQuest] scraped patNum=%s name=%q eligible=%v plan=%q accumulators=%d treatmentCodes=%d",
		sourcePatNum,
		el.Patient.FullName,
		el.Patient.IsEligible,
		el.Plan.PlanName,
		len(el.Accumulators),
		len(el.TreatmentHistory),
	)
	logging.Info("dentaquest.browser", "dentaquest.member.scrape.completed", "completed member scrape", map[string]any{
		"patNum":            sourcePatNum,
		"fullName":          el.Patient.FullName,
		"eligible":          el.Patient.IsEligible,
		"planName":          el.Plan.PlanName,
		"accumulators":      len(el.Accumulators),
		"treatmentCodes":    len(el.TreatmentHistory),
		"networkMatrixRows": len(el.NetworkMatrix),
	})
	return el, nil
}

// buildPatientEligibility creates the initial PatientEligibility skeleton from
// appointment fields.
func buildPatientEligibility(appointment models.Appointment) *eligibility.PatientEligibility {
	memberType := "Subscriber"
	if strings.EqualFold(appointment.Relationship, "dependent") {
		memberType = "Dependent"
	}

	fullName := normalizeSpace(fmt.Sprintf("%s %s", appointment.FName, appointment.LName))

	return &eligibility.PatientEligibility{
		Patient: eligibility.PatientInfo{
			FullName:    fullName,
			MemberType:  memberType,
			DateOfBirth: appointment.DOB,
			MemberID:    appointment.SubscriberID,
			IsEligible:  true,
		},
		Plan: eligibility.PlanInfo{
			Carrier:    "DentaQuest",
			Provisions: make(map[string]string),
		},
		Coverage:      eligibility.Coverage{Categories: []eligibility.CoverageCategory{}},
		NetworkTiers:  []eligibility.NetworkTier{},
		NetworkMatrix: []eligibility.NetworkMatrixRow{},
		Accumulators:  []eligibility.Accumulator{},
		OfficeSummary: []eligibility.OfficeSummaryNote{},
	}
}

func buildEmptyPatientEligibility() *eligibility.PatientEligibility {
	return &eligibility.PatientEligibility{
		Patient: eligibility.PatientInfo{
			IsEligible: true,
		},
		Plan: eligibility.PlanInfo{
			Carrier:    "DentaQuest",
			Provisions: make(map[string]string),
		},
		Coverage:      eligibility.Coverage{Categories: []eligibility.CoverageCategory{}},
		NetworkTiers:  []eligibility.NetworkTier{},
		NetworkMatrix: []eligibility.NetworkMatrixRow{},
		Accumulators:  []eligibility.Accumulator{},
		OfficeSummary: []eligibility.OfficeSummaryNote{},
	}
}
