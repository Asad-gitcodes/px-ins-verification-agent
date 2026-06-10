package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/models"

	"github.com/go-rod/rod/lib/proto"
	"golang.org/x/net/html"
)

const (
	baseURL           = "https://claimconnect.dentalxchange.com"
	eligibilityPath   = "/dci/eligibility/EligSearchPage"
	defaultUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome Safari/537.36"
	maxSnapshotBytes  = 2_500_000
	x12Self           = "18"
	x12Spouse         = "01"
	x12ChildDependent = "19"
)

type Client struct {
	http               *http.Client
	providerName       string
	providerTIN        string
	startURL           string
	forceFirstProvider bool
}

func NewClient(cookies []*proto.NetworkCookie) *Client {
	jar, _ := cookiejar.New(nil)
	if base, err := url.Parse(baseURL); err == nil {
		jar.SetCookies(base, convertCookies(cookies))
	}
	return &Client{
		http: &http.Client{
			Timeout: 45 * time.Second,
			Jar:     jar,
		},
	}
}

func (c *Client) WithProvider(providerName, providerTIN string) *Client {
	c.providerName = strings.TrimSpace(providerName)
	c.providerTIN = strings.TrimSpace(providerTIN)
	return c
}

func (c *Client) WithStartURL(startURL string) *Client {
	c.startURL = strings.TrimSpace(startURL)
	return c
}

func (c *Client) ForceFirstProvider() *Client {
	c.forceFirstProvider = true
	return c
}

func (c *Client) ProbeEligibility(ctx context.Context, appt models.Appointment) (*ProbeBundle, error) {
	searchPage, searchForm, err := c.prepareSearchForm(ctx, appt)
	if err != nil {
		return nil, err
	}

	resultPage, err := c.submitForm(ctx, searchPage.URL, searchForm, "eligibilityResult")
	if err != nil {
		return nil, err
	}
	if resultPage.HTML == "" && resultPage.Location != "" {
		resultPage, err = c.getAbsoluteWithReferer(ctx, resultPage.Location, searchPage.URL, "eligibilityResult")
		if err != nil {
			return nil, fmt.Errorf("%w payload=%s", err, safePayload(searchForm.Values))
		}
	}

	benefitsForm, err := parseBenefitsForm(resultPage.HTML)
	if err != nil {
		return nil, err
	}
	benefitsPage, err := c.submitForm(ctx, resultPage.URL, benefitsForm, "benefits")
	if err != nil {
		return nil, err
	}
	if benefitsPage.HTML == "" && benefitsPage.Location != "" {
		benefitsPage, err = c.getAbsoluteWithReferer(ctx, benefitsPage.Location, resultPage.URL, "benefits")
		if err != nil {
			return nil, fmt.Errorf("%w payload=%s", err, safePayload(benefitsForm.Values))
		}
	}

	return &ProbeBundle{
		Appointment:     appt,
		RecordedAt:      time.Now().UTC().Format(time.RFC3339),
		SearchRequest:   searchForm.Search,
		EligibilityPage: resultPage,
		BenefitsPage:    benefitsPage,
	}, nil
}

func (c *Client) PrepareSearch(ctx context.Context, appt models.Appointment) (SearchRequest, error) {
	_, searchForm, err := c.prepareSearchForm(ctx, appt)
	if err != nil {
		return SearchRequest{}, err
	}
	return searchForm.Search, nil
}

func (c *Client) prepareSearchForm(ctx context.Context, appt models.Appointment) (PageSnapshot, formPost, error) {
	searchPath := eligibilityPath
	if c.startURL != "" {
		searchPath = c.startURL
	}
	searchPage, err := c.getPage(ctx, searchPath, "eligibilitySearch")
	if err != nil {
		return PageSnapshot{}, formPost{}, err
	}
	providers, providerErr := c.FetchProviders(ctx, searchPage.URL, "")
	if providerErr != nil {
		return PageSnapshot{}, formPost{}, fmt.Errorf("provider lookup: %w", providerErr)
	}
	provider := bestProvider(providers, c.providerName, c.providerTIN, c.forceFirstProvider)
	if provider == nil || strings.TrimSpace(provider.ID) == "" {
		return PageSnapshot{}, formPost{}, fmt.Errorf("provider lookup: no provider matched name=%q tin=%q options=%d", c.providerName, c.providerTIN, len(providers))
	}
	payers, payerErr := c.FetchPayers(ctx, searchPage.URL, "")
	if payerErr != nil {
		return PageSnapshot{}, formPost{}, fmt.Errorf("payer lookup: %w", payerErr)
	}
	payer := bestPayer(payers, appt.PayerID, appt.CarrierName)
	if payer == nil || strings.TrimSpace(payer.ID) == "" {
		return PageSnapshot{}, formPost{}, fmt.Errorf("payer lookup: no payer matched payerID=%q carrier=%q options=%d", appt.PayerID, appt.CarrierName, len(payers))
	}
	searchForm, err := parseEligibilitySearchForm(searchPage.HTML, appt, provider, payer)
	if err != nil {
		return PageSnapshot{}, formPost{}, err
	}
	return searchPage, searchForm, nil
}

type LookupOption struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

func (o *LookupOption) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID   any    `json:"id"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	o.Text = raw.Text
	switch id := raw.ID.(type) {
	case string:
		o.ID = id
	case float64:
		o.ID = fmt.Sprintf("%.0f", id)
	case nil:
		o.ID = ""
	default:
		o.ID = fmt.Sprintf("%v", id)
	}
	return nil
}

type ProviderOption = LookupOption
type PayerOption = LookupOption

type providerResponse struct {
	Results []ProviderOption `json:"results"`
	More    any              `json:"more"`
}

func (c *Client) FetchProviders(ctx context.Context, pageURL string, term string) ([]ProviderOption, error) {
	u := lookupURL(pageURL, "eligibilityForm-selectProviderArea-selectProviderPanel-billingProvider")
	options, err := c.fetchLookup(ctx, pageURL, u, term, "provider")
	return []ProviderOption(options), err
}

func (c *Client) FetchPayers(ctx context.Context, pageURL string, term string) ([]PayerOption, error) {
	u := lookupURL(pageURL, "eligibilityForm-selectPayerArea-selectPayerPanel-payer")
	options, err := c.fetchLookup(ctx, pageURL, u, term, "payer")
	return []PayerOption(options), err
}

func (c *Client) fetchLookup(ctx context.Context, pageURL string, rawURL string, term string, label string) ([]LookupOption, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	q := parsed.Query()
	q.Set("term", term)
	q.Set("page", "1")
	q.Set("wicket-ajax", "true")
	q.Set("wicket-ajax-baseurl", "https://claimconnect.dentalxchange.com/dci/eligibility/EligSearchPage")
	q.Set("_", fmt.Sprintf("%d", time.Now().UnixMilli()))
	parsed.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, pageURL)
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s lookup: %w", label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s lookup failed status=%s body=%s", label, resp.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Results []LookupOption `json:"results"`
		More    any            `json:"more"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("%s lookup decode: %w", label, err)
	}
	return payload.Results, nil
}

func bestProvider(options []ProviderOption, providerName string, providerTIN string, forceFirst ...bool) *ProviderOption {
	if len(options) == 0 {
		return nil
	}
	if len(forceFirst) > 0 && forceFirst[0] {
		return &options[0]
	}
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	providerTIN = strings.TrimSpace(providerTIN)
	if providerName != "" || providerTIN != "" {
		var best *ProviderOption
		bestScore := -1
		for i := range options {
			text := strings.ToLower(options[i].Text)
			score := 0
			if providerName != "" && containsWords(text, providerName) {
				score += 10
			}
			if providerTIN != "" && strings.Contains(options[i].Text, providerTIN) {
				score += 20
			}
			if score > bestScore {
				best = &options[i]
				bestScore = score
			}
		}
		if best != nil && bestScore > 0 {
			return best
		}
	}
	for i := range options {
		text := strings.ToLower(options[i].Text)
		if strings.Contains(text, "rachna") && strings.Contains(text, "surana") && strings.Contains(text, "1912143538") {
			return &options[i]
		}
	}
	for i := range options {
		text := strings.ToLower(options[i].Text)
		if strings.Contains(text, "rachna") && strings.Contains(text, "surana") {
			return &options[i]
		}
	}
	return &options[0]
}

func providerText(provider *ProviderOption) string {
	if provider == nil {
		return ""
	}
	return provider.Text
}

func bestPayer(options []PayerOption, payerID string, carrierName string) *PayerOption {
	if len(options) == 0 {
		return nil
	}
	payerID = strings.ToLower(strings.TrimSpace(payerID))
	carrierName = strings.ToLower(strings.TrimSpace(carrierName))
	var best *PayerOption
	bestScore := -1
	for i := range options {
		text := strings.ToLower(options[i].Text)
		code := payerCode(text)
		score := 0
		if payerID != "" && code == payerID {
			score += 60
		} else if payerID != "" && strings.Contains(text, payerID) {
			score += 35
		}
		if carrierName != "" && strings.Contains(text, carrierName) {
			score += 25
			// Bonus when the payer name (before " - ") is an exact or prefix match,
			// so "CIGNA - 62308" beats "Boon Chapman (Cigna) - 62308" for carrierName "Cigna".
			payerName := strings.ToLower(strings.TrimSpace(strings.SplitN(options[i].Text, " - ", 2)[0]))
			if payerName == carrierName {
				score += 20
			} else if strings.HasPrefix(payerName, carrierName) {
				score += 10
			}
		} else if carrierName != "" && containsWords(text, carrierName) {
			score += 10
		}
		if score > bestScore {
			best = &options[i]
			bestScore = score
		}
	}
	if best != nil && bestScore > 0 {
		return best
	}
	return nil
}

func payerText(payer *PayerOption) string {
	if payer == nil {
		return ""
	}
	return payer.Text
}

func payerCode(text string) string {
	parts := strings.Split(text, " - ")
	if len(parts) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(parts[len(parts)-1]))
}

type formPost struct {
	Action string
	Values url.Values
	Search SearchRequest
}

func (c *Client) getPage(ctx context.Context, pathAndQuery string, step string) (PageSnapshot, error) {
	if strings.HasPrefix(strings.ToLower(pathAndQuery), "http://") || strings.HasPrefix(strings.ToLower(pathAndQuery), "https://") {
		return c.getAbsolute(ctx, pathAndQuery, step)
	}
	return c.getAbsolute(ctx, baseURL+pathAndQuery, step)
}

func (c *Client) getAbsolute(ctx context.Context, rawURL string, step string) (PageSnapshot, error) {
	return c.getAbsoluteWithReferer(ctx, rawURL, "", step)
}

func (c *Client) getAbsoluteWithReferer(ctx context.Context, rawURL string, referer string, step string) (PageSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resolveURL(baseURL, rawURL), nil)
	if err != nil {
		return PageSnapshot{}, err
	}
	c.setHeaders(req, referer)
	resp, err := c.http.Do(req)
	if err != nil {
		return PageSnapshot{}, fmt.Errorf("%s GET: %w", step, err)
	}
	defer resp.Body.Close()
	snap, err := pageFromResponse(resp, step)
	if err != nil {
		return snap, fmt.Errorf("%w body=%q", err, truncate(snap.Text, 800))
	}
	return snap, nil
}

func (c *Client) submitForm(ctx context.Context, referer string, form formPost, step string) (PageSnapshot, error) {
	action := resolveFormAction(referer, form.Action)
	body := []byte(form.Values.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, action, bytes.NewReader(body))
	if err != nil {
		return PageSnapshot{}, err
	}
	c.setHeaders(req, referer)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("Origin", baseURL)
	req.Header.Set("Wicket-Ajax", "true")
	req.Header.Set("Wicket-Ajax-BaseURL", wicketBaseURL(referer))
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := c.http.Do(req)
	if err != nil {
		return PageSnapshot{}, fmt.Errorf("%s POST: %w", step, err)
	}
	defer resp.Body.Close()
	snap, err := pageFromResponse(resp, step)
	if err != nil {
		return snap, fmt.Errorf("%w url=%s payload=%s body=%q", err, action, safePayload(form.Values), truncate(snap.Text, 800))
	}
	if loc := strings.TrimSpace(resp.Header.Get("Ajax-Location")); loc != "" {
		snap.Location = resolveURL(action, loc)
	}
	if snap.Location == "" {
		snap.Location = extractAjaxRedirect(snap.HTML, action)
	}
	return snap, nil
}

func (c *Client) setHeaders(req *http.Request, referer string) {
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("User-Agent", defaultUserAgent)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

func pageFromResponse(resp *http.Response, step string) (PageSnapshot, error) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotBytes))
	text := string(body)
	snap := SnapshotFromHTML(resp.Request.URL.String(), text, step)
	snap.Status = resp.Status
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return snap, fmt.Errorf("%s failed status=%s url=%s", step, resp.Status, snap.URL)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		mt, _, _ := mime.ParseMediaType(ct)
		if mt == "text/xml" && strings.TrimSpace(resp.Header.Get("Ajax-Location")) != "" {
			snap.HTML = ""
		}
	}
	return snap, nil
}

func SnapshotFromHTML(rawURL, pageHTML, step string) PageSnapshot {
	return PageSnapshot{
		URL:       rawURL,
		Title:     htmlTitle(pageHTML),
		HTML:      pageHTML,
		Text:      htmlText(pageHTML),
		Status:    "browser",
		Bytes:     len([]byte(pageHTML)),
		FetchStep: step,
	}
}

func safePayload(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		val := values.Get(key)
		lower := strings.ToLower(key)
		if strings.Contains(lower, "ssn") || strings.Contains(lower, "dob") || strings.Contains(lower, "firstname") || strings.Contains(lower, "lastname") {
			val = "<redacted>"
		}
		parts = append(parts, key+"="+val)
	}
	return strings.Join(parts, "&")
}

func truncate(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen] + "..."
}

func parseEligibilitySearchForm(pageHTML string, appt models.Appointment, provider *ProviderOption, payer *PayerOption) (formPost, error) {
	forms := parseForms(pageHTML)
	var selected *htmlForm
	for i := range forms {
		if strings.Contains(forms[i].Text, "Eligibility") ||
			hasFieldContaining(forms[i], "eligibilityIdentifyPatientPanel") ||
			strings.Contains(forms[i].Action, "eligibilityForm") {
			selected = &forms[i]
			break
		}
	}
	if selected == nil {
		return formPost{}, fmt.Errorf("eligibility search form not found")
	}
	if action := findWicketAction(pageHTML, "eligibilityForm-actionBar-actions-0-action"); action != "" {
		selected.Action = action
	}

	values := selected.Values
	memberID := strings.TrimSpace(appt.SubscriberID)
	if memberID == "" {
		memberID = strings.TrimSpace(appt.SSN)
	}
	if provider != nil && provider.ID != "" {
		setFieldSuffix(values, "billingProvider", provider.ID)
	} else {
		setFieldSuffix(values, "billingProvider", selected.firstSelectedOrFirst("billingProvider", ""))
	}
	payerValue, payerLabel := selected.selectBestOption("payer", appt.PayerID, appt.CarrierName)
	if payer != nil && payer.ID != "" {
		payerValue = payer.ID
		payerLabel = payer.Text
	}
	setFieldSuffix(values, "payer", payerValue)
	setFieldSuffix(values, "ssn", memberID)
	setFieldSuffix(values, "patientSubscriberRelationship", relationshipCode(appt.Relationship))
	setFieldSuffix(values, "patientLastName", strings.TrimSpace(appt.LName))
	setFieldSuffix(values, "patientFirstName", strings.TrimSpace(appt.FName))
	setFieldSuffix(values, "patientDob", normalizeDate(appt.DOB))
	// ClaimConnect marks Group# optional; blank matches the successful manual flow.
	setFieldSuffix(values, "groupNum", "")
	setFirstSubmit(values, selected)

	return formPost{
		Action: selected.Action,
		Values: values,
		Search: SearchRequest{
			BillingProvider: getFieldSuffix(values, "billingProvider"),
			ProviderText:    providerText(provider),
			PayerValue:      payerValue,
			PayerLabel:      firstNonEmpty(payerText(payer), payerLabel),
			MemberID:        memberID,
			PatientName:     strings.TrimSpace(appt.FName + " " + appt.LName),
			DateOfBirth:     normalizeDate(appt.DOB),
			GroupNumber:     getFieldSuffix(values, "groupNum"),
			Relationship:    getFieldSuffix(values, "patientSubscriberRelationship"),
		},
	}, nil
}

func parseBenefitsForm(pageHTML string) (formPost, error) {
	forms := parseForms(pageHTML)
	var selected *htmlForm
	for i := range forms {
		if strings.Contains(forms[i].Text, "View Benefits") ||
			strings.Contains(forms[i].Action, "benefitsSearchForm") {
			selected = &forms[i]
			break
		}
	}
	if selected == nil {
		return formPost{}, fmt.Errorf("benefits form not found")
	}
	if action := findWicketAction(pageHTML, "viewBenefitsButton"); action != "" {
		selected.Action = action
	}
	values := selected.Values
	if getFieldSuffix(values, "searchOptionRadioGroup") == "" {
		setFieldSuffix(values, "searchOptionRadioGroup", selected.firstSelectedOrFirst("searchOptionRadioGroup", ""))
	}
	setFirstSubmit(values, selected)
	return formPost{Action: selected.Action, Values: values}, nil
}

type htmlForm struct {
	Action  string
	Text    string
	Values  url.Values
	Inputs  []formControl
	Selects []selectControl
}

type formControl struct {
	Name    string
	Value   string
	Type    string
	Checked bool
}

type selectControl struct {
	Name    string
	Options []optionControl
}

type optionControl struct {
	Value    string
	Label    string
	Selected bool
}

func parseForms(pageHTML string) []htmlForm {
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return nil
	}
	var forms []htmlForm
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "form" {
			forms = append(forms, parseForm(n))
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return forms
}

func parseForm(form *html.Node) htmlForm {
	out := htmlForm{
		Action: attr(form, "action"),
		Text:   nodeText(form),
		Values: url.Values{},
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "input", "button":
				name := attr(n, "name")
				if name != "" {
					typ := strings.ToLower(attr(n, "type"))
					ctrl := formControl{Name: name, Value: attr(n, "value"), Type: typ, Checked: hasAttr(n, "checked")}
					out.Inputs = append(out.Inputs, ctrl)
					if typ == "" || typ == "hidden" || typ == "text" || typ == "radio" && ctrl.Checked {
						out.Values.Set(name, ctrl.Value)
					}
				}
			case "select":
				sel := selectControl{Name: attr(n, "name")}
				for o := n.FirstChild; o != nil; o = o.NextSibling {
					if o.Type == html.ElementNode && o.Data == "option" {
						sel.Options = append(sel.Options, optionControl{
							Value:    attr(o, "value"),
							Label:    nodeText(o),
							Selected: hasAttr(o, "selected"),
						})
					}
				}
				out.Selects = append(out.Selects, sel)
				if sel.Name != "" {
					out.Values.Set(sel.Name, sel.firstSelectedOrFirst(""))
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(form)
	return out
}

func (s selectControl) firstSelectedOrFirst(fallback string) string {
	for _, opt := range s.Options {
		if opt.Selected && strings.TrimSpace(opt.Value) != "" {
			return opt.Value
		}
	}
	for _, opt := range s.Options {
		if strings.TrimSpace(opt.Value) != "" {
			return opt.Value
		}
	}
	return fallback
}

func (f htmlForm) firstSelectedOrFirst(suffix, fallback string) string {
	for _, sel := range f.Selects {
		if strings.HasSuffix(sel.Name, suffix) || strings.Contains(sel.Name, suffix) {
			return sel.firstSelectedOrFirst(fallback)
		}
	}
	return fallback
}

func (f htmlForm) selectBestOption(suffix, payerID, carrierName string) (string, string) {
	payerID = strings.ToLower(strings.TrimSpace(payerID))
	carrierName = strings.ToLower(strings.TrimSpace(carrierName))
	for _, sel := range f.Selects {
		if !strings.HasSuffix(sel.Name, suffix) && !strings.Contains(sel.Name, suffix) {
			continue
		}
		for _, opt := range sel.Options {
			value := strings.ToLower(strings.TrimSpace(opt.Value))
			label := strings.ToLower(strings.TrimSpace(opt.Label))
			if payerID != "" && (value == payerID || strings.Contains(label, payerID)) {
				return opt.Value, opt.Label
			}
			if carrierName != "" && (strings.Contains(label, carrierName) || containsWords(label, carrierName)) {
				return opt.Value, opt.Label
			}
		}
		for _, opt := range sel.Options {
			if opt.Selected && strings.TrimSpace(opt.Value) != "" {
				return opt.Value, opt.Label
			}
		}
		for _, opt := range sel.Options {
			if strings.TrimSpace(opt.Value) != "" {
				return opt.Value, opt.Label
			}
		}
	}
	return "", ""
}

func setFieldSuffix(values url.Values, suffix, value string) {
	if value == "" {
		return
	}
	for key := range values {
		if strings.HasSuffix(key, suffix) || strings.Contains(key, suffix) {
			values.Set(key, value)
			return
		}
	}
	values.Set(suffix, value)
}

func getFieldSuffix(values url.Values, suffix string) string {
	for key, vals := range values {
		if (strings.HasSuffix(key, suffix) || strings.Contains(key, suffix)) && len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

func setFirstSubmit(values url.Values, form *htmlForm) {
	for _, in := range form.Inputs {
		if in.Name == "" {
			continue
		}
		if in.Type == "submit" || strings.Contains(strings.ToLower(in.Name), "action") ||
			strings.Contains(strings.ToLower(in.Name), "viewbenefitsbutton") {
			values.Set(in.Name, firstNonEmpty(in.Value, "1"))
			return
		}
	}
}

func hasFieldContaining(f htmlForm, needle string) bool {
	for key := range f.Values {
		if strings.Contains(key, needle) {
			return true
		}
	}
	return false
}

func convertCookies(cookies []*proto.NetworkCookie) []*http.Cookie {
	out := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		out = append(out, &http.Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Path:     c.Path,
			Domain:   c.Domain,
			Secure:   c.Secure,
			HttpOnly: c.HTTPOnly,
		})
	}
	return out
}

func resolveURL(base, ref string) string {
	if strings.TrimSpace(ref) == "" {
		return base
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func resolveFormAction(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return base
	}
	if strings.Contains(ref, "page?") && !strings.Contains(ref, "/dci/wicket/page?") {
		idx := strings.Index(ref, "page?")
		if idx >= 0 {
			return baseURL + "/dci/wicket/" + ref[idx:]
		}
	}
	return resolveURL(base, ref)
}

func lookupURL(pageURL, listener string) string {
	pageID := wicketPageID(pageURL)
	if pageID == "" {
		pageID = "0"
	}
	return fmt.Sprintf("%s/dci/wicket/page?%s-IResourceListener-%s", baseURL, pageID, listener)
}

func wicketPageID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	q := strings.TrimSpace(u.RawQuery)
	if q == "" {
		return ""
	}
	for i, r := range q {
		if r < '0' || r > '9' {
			return q[:i]
		}
	}
	return q
}

func wicketBaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := strings.TrimPrefix(u.Path, "/dci/")
	if p == "" {
		return "eligibility/EligSearchPage"
	}
	base := path.Clean(p)
	if base == "wicket/page" && u.RawQuery != "" {
		return base + "?" + u.RawQuery
	}
	return base
}

func extractAjaxRedirect(body, base string) string {
	re := regexp.MustCompile(`(?is)<redirect[^>]+url=["']([^"']+)["']`)
	if m := re.FindStringSubmatch(body); len(m) == 2 {
		return resolveURL(base, html.UnescapeString(m[1]))
	}
	return ""
}

func findWicketAction(pageHTML string, marker string) string {
	if strings.TrimSpace(marker) == "" {
		return ""
	}
	pattern := `(?i)(?:\.?/)?page\?[^"' <>\)]*IBehaviorListener[^"' <>\)]*` + regexp.QuoteMeta(marker) + `[^"' <>\)]*`
	re := regexp.MustCompile(pattern)
	match := html.UnescapeString(re.FindString(pageHTML))
	if match == "" {
		return ""
	}
	if strings.HasPrefix(match, "page?") {
		return "/dci/wicket/" + match
	}
	return match
}

func htmlTitle(pageHTML string) string {
	re := regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	if m := re.FindStringSubmatch(pageHTML); len(m) == 2 {
		return cleanText(m[1])
	}
	return ""
}

func htmlText(pageHTML string) string {
	doc, err := html.Parse(strings.NewReader(pageHTML))
	if err != nil {
		return cleanText(pageHTML)
	}
	return nodeText(doc)
}

func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch strings.ToLower(node.Data) {
			case "script", "style", "noscript":
				return
			}
		}
		if node.Type == html.TextNode {
			sb.WriteString(node.Data)
			sb.WriteByte(' ')
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return cleanText(sb.String())
}

func cleanText(s string) string {
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return true
		}
	}
	return false
}

func normalizeDate(value string) string {
	value = strings.TrimSpace(value)
	layouts := []string{"01-02-2006", "01/02/2006", "2006-01-02"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Format("01/02/2006")
		}
	}
	return value
}

func relationshipCode(value string) string {
	switch strings.TrimSpace(value) {
	case "", "0":
		return x12Self
	case "1":
		return x12Spouse
	case "2":
		return x12ChildDependent
	default:
		return x12Self
	}
}

func containsWords(haystack, needle string) bool {
	score := 0
	for _, word := range strings.Fields(needle) {
		word = strings.Trim(word, ".,()")
		if len(word) > 2 && strings.Contains(haystack, word) {
			score++
		}
	}
	return score >= 2
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
