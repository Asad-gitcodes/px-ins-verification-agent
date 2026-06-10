package models

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

type CredentialCandidate struct {
	// The server returns one selected credential per payer; fallback/office
	// selection decisions stay server-side and are exposed as metadata here.
	Slot                int    `json:"slot"`
	LoginOfficeKey      string `json:"loginOfficeKey"`
	OfficeKey           string `json:"officeKey"`
	RequestingOfficeKey string `json:"requestingOfficeKey"`
	CredentialSelection string `json:"credentialSelection"`
	Username            string `json:"username"`
	Password            string `json:"password"`
	MFAMethod           string `json:"mfaMethod"`
	MFAEmail            string `json:"mfaEmail"`
	ProviderName        string `json:"providerName"`
	ProviderTIN         string `json:"providerTin"`
}

type ScraperConfig struct {
	// Scraper config is the daily office work policy: TTL, concurrency, shared
	// APIs, MFA mailbox settings, and office-level scrape range.
	JobTTLSec          int            `json:"jobTtlSec"`
	ScraperConcurrency int            `json:"scraperConcurrency"`
	APIs               map[string]any `json:"apis"`
	MFA                MFAConfig      `json:"mfa"`
	Office             OfficeConfig   `json:"office"`
}

type MFAConfig struct {
	Email EmailMFAConfig `json:"email"`
}

type EmailMFAConfig struct {
	Host           string `json:"host"`
	Mailbox        string `json:"mailbox"`
	User           string `json:"user"`
	PollIntervalMS int    `json:"pollIntervalMs"`
	Port           int    `json:"port"`
	Secure         bool   `json:"secure"`
	TimeoutMS      int    `json:"timeoutMs"`
	Password       string `json:"password"`
}

type OfficeConfig struct {
	OfficeKey          string `json:"officeKey"`
	Active             bool   `json:"active"`
	IsTestOffice       bool   `json:"isTestOffice"`
	ApptRangeDays      int    `json:"apptRangeDays"`
	ScraperConcurrency *int   `json:"scraperConcurrency"`
	InsPDFGenerate     int    `json:"insPDFGenerate"`
	SweepIntervalMs    int    `json:"sweepIntervalMs"` // 0 = default 24h
	SweepStartTime     string `json:"sweepStartTime"`  // "HH:MM" 24h local time; empty = run immediately
}

type Payer struct {
	ID         string              `json:"_id"`
	PayerURL   string              `json:"payerUrl"`
	Name       string              `json:"name"`
	PayerIDs   []string            `json:"payerIds"`
	Credential CredentialCandidate `json:"credential"`
}

// MFAMeta holds per-payer MFA metadata from the server config.
type MFAMeta struct {
	SMS      string `json:"sms"`
	Email    string `json:"email"`
	ProvName string `json:"provName"`
	ProvTIN  string `json:"ProvTIN"`
}

// PayerConfigEntry is one entry in ServerConfig.PayerConfig.
type PayerConfigEntry struct {
	ID        string   `json:"_id"`
	UserID    int      `json:"userId"`
	PayerURL  string   `json:"payerUrl"`
	Name      string   `json:"name"`
	PayerIDs  []string `json:"payerIds"`
	Username  string   `json:"username"`
	Password  string   `json:"password"`
	MFAMethod string   `json:"mfaMethod"`
	MFAMeta   MFAMeta  `json:"mfaMeta"`
}

// ServerConfig is the full payload returned by the patcon config endpoint.
type ServerConfig struct {
	IsActive            int                `json:"IsActive"`
	UserID              int                `json:"UserId"`
	InsPDFGenerate      int                `json:"insPDFGenerate"`
	IsQuickInsScheduler int                `json:"isQuickInsScheduler"`
	QISAptRangeDays     int                `json:"qis_aptRangeDays"`
	QISIntervalMs       int                `json:"qis_intervalMs"`
	QISStartTime        string             `json:"qis_startTime"`
	QueryDomain         string             `json:"INS_SCHED_QUERY_DOMAIN"`
	QueryToken          string             `json:"INS_SCHED_QUERY_TOKEN"`
	MainDomain          string             `json:"MAIN_DOMAIN"`
	MainAPIToken        string             `json:"MAIN_API_TOKEN"`
	MFAPass             string             `json:"MFAPASS"`
	MFAUser             string             `json:"MFAUSER"`
	MFAHost             string             `json:"MFAHOST"`
	MFAPort             int                `json:"MFAPORT"`
	ConfigAPIURL        string             `json:"configApiUrl"`
	PayerConfig         []PayerConfigEntry `json:"payerConfig"`
}

// PayerTrackingRecord is POSTed to the tracking endpoint after each payer run.
type PayerTrackingRecord struct {
	LicenseKey            string    `json:"licenseKey"`
	PayerConfigID         string    `json:"payerConfigId"`
	Status                string    `json:"status"`
	ErrorReason           string    `json:"errorReason,omitempty"`
	AppointmentsTotal     int       `json:"appointmentsTotal"`
	AppointmentsProcessed int       `json:"appointmentsProcessed"`
	AppointmentsFailed    int       `json:"appointmentsFailed"`
	StartedAt             time.Time `json:"startedAt"`
	CompletedAt           time.Time `json:"completedAt"`
}

type Patient struct {
	PatNum       string `json:"patNum"`
	SubscriberID string `json:"subscriberId"`
	MemberID     string `json:"memberId"`
	DOB          string `json:"dob"`
	FullName     string `json:"fullName"`
}

type Appointment struct {
	AptNum                 string `json:"aptNum"`
	AppointmentDate        string `json:"appointmentDate"`
	PatNum                 string `json:"patNum"`
	FName                  string `json:"fName"`
	LName                  string `json:"lName"`
	DOB                    string `json:"dob"`
	SubFName               string `json:"subFName"`
	SubLName               string `json:"subLName"`
	SubZip                 string `json:"subZip"`
	SubDOB                 string `json:"subDOB"`
	SubscriberID           string `json:"subscriberId"`
	SSN                    string `json:"ssn"`
	GroupNum               string `json:"groupNum"`
	GroupName              string `json:"groupName"`
	CarrierNum             string `json:"carrierNum"`
	CarrierName            string `json:"carrierName"`
	Gender                 string `json:"gender"`
	SubGender              string `json:"subGender"`
	PayerID                string `json:"payerId"`
	Ordinal                string `json:"ordinal"`
	Relationship           string `json:"relationship"`
	InsSubNum              string `json:"insSubNum"`
	PlanNum                string `json:"planNum"`
	TreatmentPlanProcCodes string `json:"treatmentPlanProcCodes"`
}

func (a *Appointment) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	a.AptNum = stringify(raw["aptNum"])
	a.AppointmentDate = stringify(raw["appointmentDate"])
	a.PatNum = stringify(raw["patNum"])
	a.FName = stringify(raw["fName"])
	a.LName = stringify(raw["lName"])
	a.DOB = stringify(raw["dob"])
	a.SubFName = stringify(raw["subFName"])
	a.SubLName = stringify(raw["subLName"])
	a.SubZip = stringify(raw["subZip"])
	a.SubDOB = stringify(raw["subDOB"])
	a.SubscriberID = stringify(raw["subscriberId"])
	a.SSN = stringify(raw["ssn"])
	a.GroupNum = stringify(raw["groupNum"])
	a.GroupName = stringify(raw["groupName"])
	a.CarrierNum = stringify(raw["carrierNum"])
	a.CarrierName = stringify(raw["CarrierName"])
	a.Gender = stringify(raw["gender"])
	a.SubGender = stringify(raw["subgender"])
	a.PayerID = stringify(raw["payerId"])
	a.Ordinal = firstNonEmptyStringify(raw["ordinal"], raw["Ordinal"])
	a.Relationship = stringify(raw["relationship"])
	a.InsSubNum = firstNonEmptyStringify(raw["insSubNum"], raw["InsSubNum"])
	a.PlanNum = firstNonEmptyStringify(raw["planNum"], raw["PlanNum"])
	a.TreatmentPlanProcCodes = stringify(raw["treatmentPlanProcCodes"])
	return nil
}

func stringify(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return fmt.Sprint(typed)
	}
}

func firstNonEmptyStringify(values ...any) string {
	for _, value := range values {
		text := stringify(value)
		if text != "" {
			return text
		}
	}
	return ""
}
