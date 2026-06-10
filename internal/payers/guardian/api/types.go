package api

import "insurance-benefit-agent-go/internal/models"

type ProbeBundle struct {
	PayerURL       string             `json:"payerUrl,omitempty"`
	Appointment    models.Appointment `json:"appointment"`
	Search         *SearchResponse    `json:"search,omitempty"`
	Member         *MemberResponse    `json:"member,omitempty"`
	DentalPPO      *DentalPPOResponse `json:"dentalPpo,omitempty"`
	SelectedMember *SearchMember      `json:"selectedMember,omitempty"`
	RecordedAt     string             `json:"recordedAt"`
}

type SearchRequest struct {
	Identifier string `json:"identifier"`
}

type SearchResponse struct {
	Results        []SearchResult   `json:"multiple_patient_search_res"`
	SessionRequest []SessionRequest `json:"session_request"`
}

type SessionRequest struct {
	Identifier string `json:"identifier"`
}

type SearchResult struct {
	HasDuplicate         string         `json:"has_duplicate"`
	PlanType             string         `json:"plan_type"`
	TotalNumberOfRecords string         `json:"total_number_of_records"`
	InputIdentifier      string         `json:"input_identifier"`
	MaskedIdentifier     string         `json:"masked_identifier"`
	MemberDependent      []SearchMember `json:"member_dependent"`
}

type SearchMember struct {
	Sex                         string `json:"sex"`
	State                       string `json:"state"`
	City                        string `json:"city"`
	LastName                    string `json:"last_name"`
	FirstName                   string `json:"first_name"`
	GroupPolicyNumber           string `json:"group_policy_number"`
	GroupName                   string `json:"group_name"`
	DivisionID                  string `json:"division_id"`
	Identifier                  string `json:"identifier"`
	Relationship                string `json:"relationship"`
	DateOfBirth                 string `json:"date_of_birth"`
	Zip                         string `json:"zip"`
	EffectiveDate               string `json:"effective_date"`
	MemberCoverageTermDate      string `json:"member_coverage_term_date"`
	CoverageCodeMedical         string `json:"coverage_code_medical"`
	DentalCoverageEffectiveDate string `json:"dental_coverage_effective_date"`
	GRStatusCode                string `json:"gr_status_code"`
	GRTerminationDate           string `json:"gr_termination_date"`
}

type MemberSearchRequest struct {
	DivisionID string `json:"division_id"`
	GroupID    string `json:"group_id"`
	MemberID   string `json:"member_id"`
}

type MemberResponse struct {
	MemberExists                string               `json:"member_exists"`
	PartyID                     string               `json:"party_id"`
	GroupID                     string               `json:"group_id"`
	EmployeeMemberID            string               `json:"employee_member_id"`
	TaxID                       string               `json:"tax_id"`
	BillGroupIdentifier         string               `json:"bill_group_identifier"`
	EmployeeGenderCode          string               `json:"employee_gender_code"`
	EmployeeLastName            string               `json:"employee_last_name"`
	EmployeeFirstName           string               `json:"employee_first_name"`
	EmployeeMiddleName          string               `json:"employee_middle_name"`
	EmployeeBirthDate           string               `json:"employee_birth_date"`
	EmploymentStatusCode        string               `json:"employment_status_code"`
	TerminationDate             string               `json:"termination_date"`
	MemberOriginalEffectiveDate string               `json:"member_original_effective_date"`
	Dependent                   []Dependent          `json:"dependent"`
	Benefit                     []Benefit            `json:"benefit"`
	CoverageInsured             []CoverageInsured    `json:"coverage_insured"`
	SessionRequest              *MemberSearchRequest `json:"session_request,omitempty"`
}

type Dependent struct {
	PartyID                       string `json:"party_id"`
	DependentMemberID             string `json:"dependent_member_id"`
	DependentLastName             string `json:"dependent_last_name"`
	DependentFirstName            string `json:"dependent_first_name"`
	DependentMiddleName           string `json:"dependent_middle_name"`
	DependentRelationshipTypeCode string `json:"dependent_relationship_type_code"`
	DependentBirthDate            string `json:"dependent_birth_date"`
	DependentGenderCode           string `json:"dependent_gender_code"`
	DependentTerminationDate      string `json:"dependent_termination_date"`
}

type Benefit struct {
	CoverageID                    string `json:"coverage_id"`
	MemberBenefitAmount           string `json:"member_benefit_amount"`
	DependentBenefitAmount        string `json:"dependent_benefit_amount"`
	CoverageTierCode              string `json:"coverage_tier_code"`
	CoverageEffectiveDate         string `json:"coverage_effective_date"`
	CoverageTerminationDate       string `json:"coverage_termination_date"`
	CoverageCode                  string `json:"coverage_code"`
	CoverageOriginalEffectiveDate string `json:"coverage_original_effective_date"`
}

type CoverageInsured struct {
	CoverageID                     string `json:"coverage_id"`
	PartyID                        string `json:"party_id"`
	InsuredCoverageEffectiveDate   string `json:"insured_coverage_effective_date"`
	InsuredCoverageTerminationDate string `json:"insured_coverage_termination_date"`
}

type DentalPPORequest struct {
	GroupPolicyNumber       string `json:"group_policy_number"`
	PatientRelationToMember string `json:"patient_relation_to_member"`
	PatientDateOfBirth      string `json:"patient_date_of_birth"`
	PatientFirstName        string `json:"patient_first_name"`
	PatientIdentifier       string `json:"patient_identifier"`
	PatientLastName         string `json:"patient_last_name"`
}

type DentalPPOResponse struct {
	Product           Product           `json:"product"`
	PlanTypeIndicator string            `json:"plan_type_indicator"`
	PPOBenefit        []PPOBenefit      `json:"ppo_benefit"`
	MaxRollover       MaxRollover       `json:"max_rollover"`
	Patient           NamedPerson       `json:"patient"`
	Member            MemberSummary     `json:"member"`
	AgeLimit          []AgeLimit        `json:"age_limt"`
	NetworkConfigCode string            `json:"network_config_code"`
	SessionRequest    *DentalPPORequest `json:"session_request,omitempty"`
}

type Product struct {
	ProductType string `json:"product_type"`
	ProductName string `json:"product_name"`
}

type PPOBenefit struct {
	PlanMaximum        []PlanMaximum      `json:"plan_maximum"`
	BenefitInformation BenefitInformation `json:"benefit_information"`
	Deductible         []Deductible       `json:"deductible"`
	PlanOption         []PlanOption       `json:"plan_option"`
}

type PlanMaximum struct {
	TimeQualifier         string `json:"time_qualifier"`
	PlanMaximumForBenefit string `json:"plan_maximum_for_benefit"`
	Amount                string `json:"amount"`
	NetworkName           string `json:"network_name"`
}

type BenefitInformation struct {
	ServiceCategoryEffectiveDate []ServiceCategoryEffectiveDate `json:"service_category_effective_date"`
	BenefitPlanType              string                         `json:"benefit_plan_type"`
	BenefitPeriodEffectiveDate   string                         `json:"benefit_period_effective_date"`
	BenefitPeriodEndDate         string                         `json:"benefit_period_end_date"`
}

type ServiceCategoryEffectiveDate struct {
	DentalServiceCategory      string `json:"dental_service_category"`
	EffectiveDate              string `json:"effective_date"`
	OutNetworkDeductibleWaived string `json:"out_network_deductible_waived"`
	InNetworkDeductibleWaived  string `json:"in_network_deductible_waived"`
	LateEntrantIndicator       string `json:"late_entrant_indicator"`
}

type Deductible struct {
	Amount           string `json:"amount"`
	CoverageTier     string `json:"coverage_tier"`
	NetworkName      string `json:"network_name"`
	DeductiblePeriod string `json:"deductible_period"`
}

type PlanOption struct {
	Category         []Category    `json:"category"`
	DentalService    string        `json:"dental_service"`
	LastVisitDate    string        `json:"last_visit_date"`
	Message          []string      `json:"message"`
	Coinsurance      []Coinsurance `json:"coinsurance"`
	EHBPlanIndicator string        `json:"ehb_plan_indicator"`
}

type Category struct {
	CategoryType string `json:"category_type"`
}

type Coinsurance struct {
	CoinsuranceAmount string `json:"coinsurance_amount"`
	NetworkName       string `json:"network_name"`
}

type MaxRollover struct {
	MaximumRolloverAmount      string `json:"maximum_rollover_amount"`
	MaxrolloverAmount          string `json:"maxrollover_amount"`
	Threshold                  string `json:"threshold"`
	RolloverAmountPaidBenefits string `json:"rollover_amount_paid_benefits"`
	MaximumRolloverAccountMax  string `json:"maximum_rollover_account_max"`
}

type NamedPerson struct {
	LastName  string `json:"last_name"`
	FirstName string `json:"first_name"`
	Relation  string `json:"relation"`
}

type MemberSummary struct {
	GroupPolicyNumber string `json:"group_policy_number"`
	DateOfBirth       string `json:"date_of_birth"`
	LastName          string `json:"last_name"`
	OrganizationName  string `json:"organization_name"`
	FirstName         string `json:"first_name"`
}

type AgeLimit struct {
	BenefitCategory string `json:"benefit_category"`
	Age             string `json:"age"`
}
