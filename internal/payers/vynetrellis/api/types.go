package api

// LoginRequest is the payload for POST /services/authentication/credentials/SsoLogin.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResponse is returned by the Vyne Trellis login endpoint.
type LoginResponse struct {
	AccessToken  string `json:"accessToken"`
	ExpiresInSec int    `json:"expiresInSeconds"`
	AuthToken    string `json:"authToken"`
	CustomerID   int    `json:"customerId"`
}

// PracticeResponse is returned by GET /services/Practice?customerId={id}.
type PracticeResponse struct {
	Info       TrellisPracticeInfo `json:"trellisPracticeInfo"`
	StatusCode int                 `json:"statusCode"`
	Message    *string             `json:"message"`
}

type TrellisPracticeInfo struct {
	ProviderLastName  string `json:"providerLastName"`
	ProviderFirstName string `json:"providerFirstName"`
	ProviderNPI       string `json:"providerNPI"`
	TaxonomyCode      string `json:"taxonomyCode"`
	OfficeName        string `json:"officeName"`
}

// PatientSearchRequest is the payload for POST /trellis-eligibility/{customerId}/patients.
type PatientSearchRequest struct {
	CurrentPage int            `json:"CurrentPage"`
	Filters     PatientFilters `json:"Filters"`
	PageSize    int            `json:"PageSize"`
	SortColumn  SortColumn     `json:"SortColumn"`
}

type PatientFilters struct {
	Name string `json:"Name,omitempty"`
}

type SortColumn struct {
	Column string `json:"Column"`
	Sort   string `json:"Sort"`
}

// PatientSearchResponse is returned by the patient list endpoint.
type PatientSearchResponse struct {
	TotalCount int              `json:"TotalCount"`
	Data       []PatientSummary `json:"Data"`
}

// PatientSummary is one row in the patient search results.
type PatientSummary struct {
	PatientId        string  `json:"PatientId"`
	SyncId           string  `json:"SyncId"`
	PatientFirstName string  `json:"PatientFirstName"`
	PatientLastName  string  `json:"PatientLastName"`
	PatientBirthdate string  `json:"PatientBirthdate"`
	CarrierName      string  `json:"CarrierName"`
	CarrierId        string  `json:"CarrierId"`
	MemberId         string  `json:"MemberId"`
	Status           *string `json:"Status"`
	HtmlResult       *string `json:"HtmlResult"`
	ResponseError    *string `json:"ResponseError"`
	RequestError     *string `json:"RequestError"`
}

// PatientDetail is returned by GET /trellis-eligibility/{customerId}/patient/{patientId}.
type PatientDetail struct {
	VerificationHistory      []VerificationRecord `json:"VerificationHistory"`
	PatientId                string               `json:"PatientId"`
	SyncId                   string               `json:"SyncId"`
	CustomerId               int                  `json:"CustomerId"`
	PatientFirstName         string               `json:"PatientFirstName"`
	PatientMiddleName        *string              `json:"PatientMiddleName"`
	PatientLastName          string               `json:"PatientLastName"`
	PatientSuffix            *string              `json:"PatientSuffix"`
	PatientBirthdate         string               `json:"PatientBirthdate"`
	PatientGender            string               `json:"PatientGender"`
	PatientIsSub             bool                 `json:"PatientIsSub"`
	SubscriberId             string               `json:"SubscriberId"`
	SubscriberFirstName      string               `json:"SubscriberFirstName"`
	SubscriberMiddleName     *string              `json:"SubscriberMiddleName"`
	SubscriberLastName       string               `json:"SubscriberLastName"`
	SubscriberSuffix         *string              `json:"SubscriberSuffix"`
	SubscriberBirthdate      string               `json:"SubscriberBirthdate"`
	SubscriberGender         string               `json:"SubscriberGender"`
	CarrierName              string               `json:"CarrierName"`
	GroupNumber              string               `json:"GroupNumber"`
	CarrierId                string               `json:"CarrierId"`
	ProviderLastName         string               `json:"ProviderLastName"`
	ProviderFirstName        string               `json:"ProviderFirstName"`
	IndividualNpi            string               `json:"IndividualNpi"`
	TaxonomyCode             string               `json:"TaxonomyCode,omitempty"`
	RelationshipToSubscriber *string              `json:"RelationshipToSubscriber"`
	CarrierMasterId          *string              `json:"CarrierMasterId"`
	Status                   *string              `json:"Status"`
}

// VerifyRequest is the payload for POST /trellis-eligibility/{customerId}/verify/0.
// All demographic fields come from PatientDetail.
type VerifyRequest struct {
	PatientId            string  `json:"PatientId"`
	CustomerId           int     `json:"CustomerId"`
	PatientFirstName     string  `json:"PatientFirstName"`
	PatientMiddleName    *string `json:"PatientMiddleName"`
	PatientLastName      string  `json:"PatientLastName"`
	PatientSuffix        *string `json:"PatientSuffix"`
	PatientBirthdate     string  `json:"PatientBirthdate"`
	PatientGender        string  `json:"PatientGender"`
	PatientIsSub         bool    `json:"PatientIsSub"`
	SubscriberId         string  `json:"SubscriberId"`
	SubscriberFirstName  string  `json:"SubscriberFirstName"`
	SubscriberMiddleName *string `json:"SubscriberMiddleName"`
	SubscriberLastName   string  `json:"SubscriberLastName"`
	SubscriberSuffix     *string `json:"SubscriberSuffix"`
	SubscriberBirthdate  string  `json:"SubscriberBirthdate"`
	SubscriberGender     string  `json:"SubscriberGender"`
	CarrierId            string  `json:"CarrierId"`
	CarrierName          string  `json:"CarrierName"`
	GroupNumber          string  `json:"GroupNumber"`
	IndividualNpi        string  `json:"IndividualNpi"`
	ProviderFirstName    string  `json:"ProviderFirstName"`
	ProviderLastName     string  `json:"ProviderLastName"`
	SyncId               string  `json:"SyncId"`
	TaxonomyCode         string  `json:"TaxonomyCode,omitempty"`
}

// VerificationRecord is one entry in PatientDetail.VerificationHistory.
type VerificationRecord struct {
	HistoryId     *int    `json:"HistoryId"`
	CarrierName   string  `json:"CarrierName"`
	GroupNumber   string  `json:"GroupNumber"`
	RequestDate   string  `json:"RequestDate"`
	Status        *string `json:"Status"`
	ResponseDate  *string `json:"ResponseDate"`
	ResponseError *string `json:"ResponseError"`
	HtmlResult    *string `json:"HtmlResult"`
}

// VerifyResponse is returned by POST /trellis-eligibility/{customerId}/verify/0.
type VerifyResponse struct {
	HtmlResult            *string `json:"HtmlResult"`
	EligibilityId         int     `json:"EligibilityId"`
	StatusCode            int     `json:"StatusCode"`
	Status                string  `json:"Status"`
	StatusDescription     *string `json:"StatusDescription"`
	ResponseErrorMessage  *string `json:"ResponseErrorMessage"`
	PatientId             int     `json:"PatientId"`
	StructuredViewEnabled bool    `json:"StructuredViewEnabled"`
}

// ProbeBundle is written to disk as the Phase 1 artifact.
// It carries everything needed for Phase 2 without hitting the network again.
type ProbeBundle struct {
	OriginalPayerID string          `json:"originalPayerId"`
	Patient         *PatientDetail  `json:"patient"`
	Verification    *VerifyResponse `json:"verification"`
}
