package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-rod/rod"
)

const baseURL = "https://dentalprovider.metlife.com"

type Probe struct {
	page *rod.Page
}

type EligibilityOverviewRequest struct {
	EmployeeID   string `json:"employeeId"`
	PlanTypeCode string `json:"planTypeCode"`
	LastName     string `json:"lastName,omitempty"`
	ZipCode      string `json:"zipCode,omitempty"`
}

type PlanOverviewRequest struct {
	EmployeeID              string `json:"employeeId"`
	GroupNumber             string `json:"groupNumber"`
	CustomerNumber          string `json:"customerNumber"`
	Branch                  string `json:"branch"`
	SubDivision             string `json:"subDivision"`
	DependentSequenceNumber string `json:"dependentSequenceNumber"`
	RelationshipCode        string `json:"relationshipCode"`
	PPOInd                  string `json:"ppoInd"`
}

type ProcedureCategoriesRequest struct {
	EmployeeID     string `json:"employeeId"`
	CustomerNumber string `json:"customerNumber"`
	PlanTypeCode   string `json:"planTypeCode"`
}

type ProcedureCodesRequest struct {
	Branch                  string `json:"branch"`
	DependentSequenceNumber string `json:"dependentSequenceNumber"`
	EmployeeID              string `json:"employeeId"`
	Group                   string `json:"group"`
	KeyNum                  string `json:"keyNum"`
	PPOInd                  string `json:"ppoInd"`
	ProcedureCategory       string `json:"procedureCategory"`
	ProviderFirstInitial    string `json:"providerFirstInitial"`
	ProviderLastname        string `json:"providerLastname"`
	ProviderPhone           string `json:"providerPhone"`
	ProviderState           string `json:"providerState"`
	ProviderTin             string `json:"providerTin"`
	ProviderUnique          string `json:"providerUnique"`
	ProviderZipcode         string `json:"providerZipcode"`
	RelationshipCode        string `json:"relationshipCode"`
	SubDivision             string `json:"subDivision"`
}

type ProvidersRequest struct {
	ActualID       string `json:"actualId"`
	CustomerNumber string `json:"customerNumber"`
	SSN            string `json:"ssn"`
	TIN            string `json:"tin"`
}

type EligibilityOverviewResponse struct {
	CoveredPersons []CoveredPerson `json:"coveredPersons"`
	ReasonCode     string          `json:"reasonCode"`
	ReasonMessage  string          `json:"reasonMessage"`
	ReturnCode     string          `json:"returnCode"`
	MetaData       struct {
		ServiceReferenceID string `json:"serviceReferenceId"`
		Outcome            struct {
			StatusCode        int    `json:"statusCode"`
			Message           string `json:"message"`
			AdditionalDetails any    `json:"additionalDetails"`
		} `json:"outcome"`
	} `json:"metaData"`
}

type CoveredPerson struct {
	ActualID                    string `json:"actualId"`
	FirstName                   string `json:"firstName"`
	MiddleName                  string `json:"middleName"`
	LastName                    string `json:"lastName"`
	Employer                    string `json:"employer"`
	DateOfBirth                 string `json:"dateOfBirth"`
	RelationShipCode            string `json:"relationShipCode"`
	RelationShipCodeDescription string `json:"relationShipCodeDescription"`
	DependentSequenceNumber     string `json:"dependentSequenceNumber"`
	CoverageStartDate           string `json:"coverageStartDate"`
	CoverageEndDate             string `json:"coverageEndDate"`
	City                        string `json:"city"`
	State                       string `json:"state"`
	Zip                         string `json:"zip"`
	CountryCode                 string `json:"countryCode"`
	Address                     string `json:"address"`
	GroupNumber                 string `json:"groupNumber"`
	SubDivision                 string `json:"subDivision"`
	Branch                      string `json:"branch"`
	SSN                         string `json:"ssn"`
	Gender                      string `json:"gender"`
	CustomerNumber              string `json:"customerNumber"`
	CoverageStatus              string `json:"coverageStatus"`
	CoverageIndicator           string `json:"coverageIndicator"`
	CoverageType                string `json:"coverageType"`
	Plan                        string `json:"plan"`
	EmployeeID                  string `json:"employeeId"`
	Network                     string `json:"network"`
	NetworkID                   string `json:"networkId"`
	KeyNumID                    string `json:"keyNumId"`
	FlipPlan                    string `json:"flipPlan"`
	CoPayType                   string `json:"coPayType"`
	POSInd                      string `json:"pOSInd"`
	TierOneInd                  string `json:"tierOneInd"`
	TierTwoInd                  string `json:"tierTwoInd"`
	TierThreeInd                string `json:"tierThreeInd"`
	TierFourInd                 string `json:"tierFourInd"`
	TypeSched                   string `json:"typeSched"`
	PlanDisplayIndicator        bool   `json:"planDisplayIndicator"`
	MemberStatusCode            string `json:"memberStatusCode"`
	CoverageDisplayIndicator    bool   `json:"coverageDisplayIndicator"`
}

type PlanOverviewResponse struct {
	Maximums struct {
		Annual struct {
			TotalAmount     string `json:"totalAmount"`
			UsedAmount      string `json:"usedAmount"`
			RemainingAmount string `json:"remainingAmount"`
		} `json:"annual"`
		Lifetime struct {
			Constraints []struct {
				ConstraintType  string `json:"constraintType"`
				TotalAmount     string `json:"totalAmount"`
				UsedAmount      string `json:"usedAmount"`
				RemainingAmount string `json:"remainingAmount"`
			} `json:"constraints"`
		} `json:"lifetime"`
	} `json:"maximums"`
	Plan struct {
		FlipPlan       string `json:"flipPlan"`
		PlanCode       string `json:"planCode"`
		ExcessRiskCode string `json:"excessRiskCode"`
	} `json:"plan"`
	IncentiveData struct {
		IncentiveInd                   bool     `json:"incentiveInd"`
		IncentiveDesc                  string   `json:"incentiveDesc"`
		CoinsLevel                     int      `json:"coinsLevel"`
		SkippedYears                   int      `json:"skippedYears"`
		LevelsDropped                  int      `json:"levelsDropped"`
		LevelsDroppedDesc              string   `json:"levelsDroppedDesc"`
		UtilizationAmount              string   `json:"utilizationAmount"`
		PlanIncentiveInd               string   `json:"planIncentiveInd"`
		MaxIncrAmt                     string   `json:"maxIncrAmt"`
		IncEligProcedureCode           []string `json:"incEligProcedureCode"`
		PatientMaxIncrAmt              int      `json:"patientMaxIncrAmt"`
		PatientDeductDecrAmt           int      `json:"patientDeductDecrAmt"`
		PatientCoinsIncrAmt            any      `json:"patientCoinsIncrAmt"`
		IncCount                       int      `json:"incCount"`
		IncMaxMet                      string   `json:"incMaxMet"`
		IsBenefitsConnected            string   `json:"isBenefitsConnected"`
		NumberOfVisits                 string   `json:"numberOfVisits"`
		MaxPlanIncentiveIncreases      string   `json:"maxPlanIncentiveIncreases"`
		AmountCoinsuranceIncPercentage string   `json:"amountCoinsuranceIncPercentage"`
		DecAnnualAmount                string   `json:"decAnnualAmount"`
		PreAuthRequired                string   `json:"preAuthRequired"`
		PlanStartDate                  string   `json:"planStartDate"`
	} `json:"incentiveData"`
	Deductibles struct {
		Individual struct {
			TotalAmount     string `json:"totalAmount"`
			UsedAmount      string `json:"usedAmount"`
			RemainingAmount string `json:"remainingAmount"`
		} `json:"individual"`
		Family struct {
			TotalAmount     string `json:"totalAmount"`
			UsedAmount      string `json:"usedAmount"`
			RemainingAmount string `json:"remainingAmount"`
		} `json:"family"`
	} `json:"deductibles"`
	PlanProvisions []struct {
		Label string   `json:"label"`
		Text  []string `json:"text"`
	} `json:"planProvisions"`
	CoPayIndicator     bool `json:"coPayIndicator"`
	ClaimCenterAddress struct {
		AddressLine1 string `json:"addressLine1"`
		City         string `json:"city"`
		StateCode    string `json:"stateCode"`
		PostalCode   string `json:"postalCode"`
		Phone        string `json:"phone"`
	} `json:"claimCenterAddress"`
	MetaData struct {
		ServiceReferenceID string `json:"serviceReferenceId"`
		Outcome            struct {
			StatusCode        int    `json:"statusCode"`
			Message           string `json:"message"`
			AdditionalDetails any    `json:"additionalDetails"`
		} `json:"outcome"`
	} `json:"metaData"`
}

type ProcedureCategoriesResponse struct {
	Insureds []ProcedureCategoriesInsured `json:"insureds"`
	MetaData struct {
		ServiceReferenceID string `json:"serviceReferenceId"`
		Outcome            struct {
			StatusCode        int    `json:"statusCode"`
			Message           string `json:"message"`
			AdditionalDetails any    `json:"additionalDetails"`
		} `json:"outcome"`
	} `json:"metaData"`
}

type ProcedureCodesResponse struct {
	Procedures []ProcedureCodeBenefit `json:"procedures"`
	MetaData   struct {
		ServiceReferenceID string `json:"serviceReferenceId"`
		Outcome            struct {
			StatusCode        int    `json:"statusCode"`
			Message           string `json:"message"`
			AdditionalDetails any    `json:"additionalDetails"`
		} `json:"outcome"`
	} `json:"metaData"`
}

type ProvidersResponse struct {
	Providers []Provider `json:"providers"`
	MetaData  struct {
		ServiceReferenceID string `json:"serviceReferenceId"`
		Outcome            struct {
			StatusCode        int    `json:"statusCode"`
			Message           string `json:"message"`
			AdditionalDetails any    `json:"additionalDetails"`
		} `json:"outcome"`
	} `json:"metaData"`
}

type ProcedureCategoriesInsured struct {
	InsuredNumber           string                   `json:"insuredNumber"`
	PlanType                string                   `json:"planType"`
	InNetworkKeyNumber      string                   `json:"inNetworkKeyNumber"`
	OutNetworkKeyNumber     string                   `json:"outNetworkKeyNumber"`
	ProcedureCategoryGroups []ProcedureCategoryGroup `json:"procedureCategoryGroups"`
}

type ProcedureCategoryGroup struct {
	CategoryGroupName   string                        `json:"categoryGroupName"`
	BenefitLevelRange   []BenefitLevelRange           `json:"benefitLevelRange"`
	Deductibles         []ProcedureCategoryDeductible `json:"deductibles"`
	ProcedureCategories []ProcedureCategory           `json:"procedureCategories"`
}

type BenefitLevelRange struct {
	NetworkTypeCode string `json:"networkTypeCode"`
	Range           string `json:"range"`
	CoPayIndicator  bool   `json:"coPayIndicator"`
}

type ProcedureCategoryDeductible struct {
	NetworkTypeCode      string   `json:"networkTypeCode"`
	DeductibleApplies    []string `json:"deductibleApplies"`
	DeductibleNotApplies []string `json:"deductibleNotApplies"`
}

type ProcedureCategory struct {
	TypeCode          string                   `json:"typeCode"`
	Name              string                   `json:"name"`
	LimitsDescription string                   `json:"limitsDescription"`
	LastServiceDate   any                      `json:"lastServiceDate"`
	AgeLimit          string                   `json:"ageLimit"`
	BenefitDetails    []ProcedureBenefitDetail `json:"benefitDetails"`
}

type ProcedureBenefitDetail struct {
	NetworkTypeCode    string `json:"networkTypeCode"`
	BenefitLevelDesc   string `json:"benefitLevelDesc"`
	CoPayIndicator     bool   `json:"coPayIndicator"`
	PercentPpo         string `json:"percentPpo"`
	DeductibleDesc     string `json:"deductibleDesc"`
	DeductibleTypeCode string `json:"deductibleTypeCode"`
	LimitsDescription  string `json:"limitsDescription"`
	AgeLimit           string `json:"ageLimit"`
}

type ProcedureCodeBenefit struct {
	TypeCode          string `json:"typeCode"`
	Description       string `json:"description"`
	NetworkFee        string `json:"networkFee"`
	PatientObligation string `json:"patientObligation"`
	BenefitLevel      string `json:"benefitLevel"`
	CoPayInd          bool   `json:"coPayInd"`
}

type Provider struct {
	FirstName        string      `json:"firstName"`
	LastName         string      `json:"lastName"`
	ProviderName     string      `json:"providerName"`
	PracticeName     string      `json:"practiceName"`
	ZipCode          string      `json:"zipCode"`
	NetworkIndicator string      `json:"networkIndicator"`
	NetworkTypeCode  string      `json:"networkTypeCode"`
	VendorTypeCode   string      `json:"vendorTypeCode"`
	ProviderKey      ProviderKey `json:"providerKey"`
}

type ProviderKey struct {
	Phone        string `json:"phone"`
	State        string `json:"state"`
	LastName     string `json:"lastName"`
	FirstInitial string `json:"firstInitial"`
	UniqueNumber int    `json:"uniqueNumber"`
}

func NewProbe(page *rod.Page) (*Probe, error) {
	if page == nil {
		return nil, fmt.Errorf("MetLife probe requires a browser page")
	}
	return &Probe{page: page}, nil
}

// browserPost executes a POST request from within the browser page so that
// Chrome's TLS stack, cookies, and Sec-Fetch headers are used automatically.
// This bypasses Akamai bot-protection that blocks Go's http.Client.
func (p *Probe) browserPost(ctx context.Context, path string, body []byte, dest interface{}) error {
	timeout := 30 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d > 0 && d < timeout {
			timeout = d
		}
	}

	res, err := p.page.Timeout(timeout).Eval(`
		async (url, bodyStr) => {
			const resp = await fetch(url, {
				method: 'POST',
				headers: {
					'Content-Type': 'application/json',
					'Accept': 'application/json, text/plain, */*',
				},
				body: bodyStr,
				credentials: 'include',
			});
			if (!resp.ok) {
				const text = await resp.text();
				throw new Error('HTTP ' + resp.status + ': ' + text.slice(0, 500));
			}
			return resp.json();
		}
	`, baseURL+path, string(body))
	if err != nil {
		return err
	}
	return res.Value.Unmarshal(dest)
}

func (p *Probe) FetchEligibilityOverview(ctx context.Context, req EligibilityOverviewRequest) (*EligibilityOverviewResponse, error) {
	if p == nil || p.page == nil {
		return nil, fmt.Errorf("MetLife probe is not initialized")
	}
	if req.EmployeeID == "" {
		return nil, fmt.Errorf("employeeId is required")
	}
	if req.PlanTypeCode == "" {
		return nil, fmt.Errorf("planTypeCode is required")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal MetLife overview request: %w", err)
	}
	var parsed EligibilityOverviewResponse
	if err := p.browserPost(ctx, "/md2/v1/metdental/eligibility/overview", payload, &parsed); err != nil {
		return nil, fmt.Errorf("MetLife overview: %w", err)
	}
	return &parsed, nil
}

func (p *Probe) FetchPlanOverview(ctx context.Context, req PlanOverviewRequest) (*PlanOverviewResponse, error) {
	if p == nil || p.page == nil {
		return nil, fmt.Errorf("MetLife probe is not initialized")
	}
	if req.EmployeeID == "" {
		return nil, fmt.Errorf("employeeId is required")
	}
	if req.GroupNumber == "" {
		return nil, fmt.Errorf("groupNumber is required")
	}
	if req.CustomerNumber == "" {
		return nil, fmt.Errorf("customerNumber is required")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal MetLife plan overview request: %w", err)
	}
	var parsed PlanOverviewResponse
	if err := p.browserPost(ctx, "/md2/v1/metdental/eligibility/planOverview", payload, &parsed); err != nil {
		return nil, fmt.Errorf("MetLife plan overview: %w", err)
	}
	return &parsed, nil
}

func (p *Probe) FetchProcedureCategories(ctx context.Context, req ProcedureCategoriesRequest) (*ProcedureCategoriesResponse, error) {
	if p == nil || p.page == nil {
		return nil, fmt.Errorf("MetLife probe is not initialized")
	}
	if req.EmployeeID == "" {
		return nil, fmt.Errorf("employeeId is required")
	}
	if req.CustomerNumber == "" {
		return nil, fmt.Errorf("customerNumber is required")
	}
	if req.PlanTypeCode == "" {
		return nil, fmt.Errorf("planTypeCode is required")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal MetLife procedure categories request: %w", err)
	}
	var parsed ProcedureCategoriesResponse
	if err := p.browserPost(ctx, "/md2/v1/metdental/eligibility/procedureCategories", payload, &parsed); err != nil {
		return nil, fmt.Errorf("MetLife procedure categories: %w", err)
	}
	return &parsed, nil
}

func (p *Probe) FetchProcedureCodes(ctx context.Context, req ProcedureCodesRequest) (*ProcedureCodesResponse, error) {
	if p == nil || p.page == nil {
		return nil, fmt.Errorf("MetLife probe is not initialized")
	}
	if req.EmployeeID == "" {
		return nil, fmt.Errorf("employeeId is required")
	}
	if req.ProcedureCategory == "" {
		return nil, fmt.Errorf("procedureCategory is required")
	}
	if req.Group == "" {
		return nil, fmt.Errorf("group is required")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal MetLife procedure codes request: %w", err)
	}
	var parsed ProcedureCodesResponse
	if err := p.browserPost(ctx, "/md2/v1/metdental/eligibility/procedureCodes", payload, &parsed); err != nil {
		return nil, fmt.Errorf("MetLife procedure codes: %w", err)
	}
	return &parsed, nil
}

func (p *Probe) FetchProviders(ctx context.Context, req ProvidersRequest) (*ProvidersResponse, error) {
	if p == nil || p.page == nil {
		return nil, fmt.Errorf("MetLife probe is not initialized")
	}
	if req.ActualID == "" {
		return nil, fmt.Errorf("actualId is required")
	}
	if req.CustomerNumber == "" {
		return nil, fmt.Errorf("customerNumber is required")
	}
	if req.TIN == "" {
		return nil, fmt.Errorf("tin is required")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal MetLife providers request: %w", err)
	}
	var parsed ProvidersResponse
	if err := p.browserPost(ctx, "/md2/v1/metdental/eligibility/providers", payload, &parsed); err != nil {
		return nil, fmt.Errorf("MetLife providers: %w", err)
	}
	return &parsed, nil
}
