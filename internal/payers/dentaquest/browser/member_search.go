package browser

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/logging"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const memberEligibilityURL = "https://providers.dentaquest.com/member-eligibility-search/"

// secondDuration is a convenience alias used by provider.go too.
const secondDuration = time.Second

type SearchSummary struct {
	BatchSize              int
	SuccessCount           int
	NotFound               int
	Errored                int
	SuccessfulAppointments []models.Appointment
}

func (s *Session) PrepareInitialMemberSearch(input payers.SessionInput, isFirstBatch bool) (*SearchSummary, error) {
	if s == nil || s.browser == nil || s.browser.Page == nil {
		return nil, fmt.Errorf("browser session is not initialized")
	}

	page := s.browser.Page
	batch := input.Appointments
	if len(batch) > 10 {
		batch = batch[:10]
	}
	if len(batch) == 0 {
		return nil, fmt.Errorf("DentaQuest member search requires at least one appointment")
	}
	logging.Info("dentaquest.browser", "dentaquest.batch.search_prepare_started", "preparing initial member search", map[string]any{
		"batchSize": len(batch),
	})

	if isFirstBatch {
		if err := openMemberSearchFromNavigation(page); err != nil {
			return nil, err
		}
	}

	// if err := openMemberSearchFromNavigation(page); err != nil {
	// 	return nil, err
	// }
	// log.Println("Completed and redirected to Search page")
	// logging.Info("dentaquest.browser", "dentaquest.batch.search_open_completed", "opened member search", map[string]any{
	// 	"currentURL": currentURL(page),
	// })

	if err := ensureSearchMembersModalVisible(page); err != nil {
		return nil, err
	}

	if err := ensureSearchMembersProviderSelected(page, input.Credential.ProviderName); err != nil {
		return nil, err
	}

	if err := ensureSearchLines(page, len(batch)); err != nil {
		return nil, err
	}

	for index, appointment := range batch {
		if err := fillMemberSearchRowAtIndex(page, index, appointment); err != nil {
			return nil, err
		}
	}
	if len(batch) == 10 {
		time.Sleep(5 * time.Second)
	}

	if err := waitForSearchStatuses(page); err != nil {
		return nil, err
	}
	time.Sleep(2 * time.Second)

	summary := inspectSearchStatuses(page, batch)
	log.Printf(
		"[DentaQuest] initial member search prepared: rows=%d successful=%d notFound=%d error=%d",
		summary.BatchSize, summary.SuccessCount, summary.NotFound, summary.Errored,
	)
	if err := waitForAddResultsButtonCount(page, summary.SuccessCount, 5*time.Second); err != nil {
		log.Printf("[DentaQuest] add-results button count did not settle to expected=%d: %v", summary.SuccessCount, err)
	}

	if err := clickAddSuccessfulResults(page, summary.SuccessCount); err != nil {
		return nil, err
	}
	return summary, nil
}

func openMemberSearchFromNavigation(page *rod.Page) error {
	// Dismiss any visible notification banner.
	if closeNotif, err := page.Element(`button[aria-label="Close notification"]`); err == nil {
		if visible, _ := closeNotif.Visible(); visible {
			if err := closeNotif.Click(proto.InputMouseButtonLeft, 1); err != nil {
				log.Printf("[DentaQuest] close notification click failed: %v", err)
			}
		}
	}

	members, err := page.Element(`[data-testid="navigation-item-members"]`)
	if err != nil {
		return fmt.Errorf("members nav item not found: %w", err)
	}
	pt, err := elementCenter(members)
	if err != nil {
		return fmt.Errorf("members center point: %w", err)
	}
	if err := page.Mouse.MoveTo(pt); err != nil {
		return fmt.Errorf("mouse move to members failed: %w", err)
	}
	submenu, err := page.Timeout(3 * time.Second).Element(`[data-testid="navigation-submenu-link-new-search"]`)
	if err != nil {
		return fmt.Errorf("new search submenu not found: %w", err)
	}
	subPt, err := elementCenter(submenu)
	if err != nil {
		return fmt.Errorf("submenu center point: %w", err)
	}
	if err := page.Mouse.MoveTo(pt); err != nil { // re-hover members to keep submenu visible
		return fmt.Errorf("mouse re-hover members: %w", err)
	}
	if err := page.Mouse.MoveTo(subPt); err != nil {
		return fmt.Errorf("mouse move to submenu: %w", err)
	}
	if err := page.Mouse.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("mouse click on new search failed: %w", err)
	}
	return nil
}

func ensureSearchMembersModalVisible(page *rod.Page) error {
	// log.Println("Inside ensureSearchMembersModalVisible()")

	// const providerBtnSel = `button[data-testid="select-input_select-btn"][aria-label^="Provider"]`

	// providerVisible := false
	// providerBtn, err := page.Timeout(10 * time.Second).Element(providerBtnSel)
	// if err == nil {
	// 	if visible, _ := providerBtn.Visible(); visible {
	// 		providerVisible = true
	// 		label, _ := providerBtn.Attribute("aria-label")
	// 		if label != nil {
	// 			log.Printf("[DentaQuest] modal provider label=%q", *label)
	// 		} else {
	// 			log.Printf("[DentaQuest] modal provider button found without aria-label")
	// 		}
	// 	} else {
	// 		log.Printf("[DentaQuest] modal provider button found but not visible")
	// 	}
	// } else {
	// 	log.Printf("[DentaQuest] modal provider button not found: %v", err)
	// }

	// firstRowInput, err := page.Element(`[data-testid="member-row-1-member-id-input"]`)
	// if err == nil {
	// 	if visible, _ := firstRowInput.Visible(); visible {
	// 		if providerVisible {
	// 			log.Println("[DentaQuest] search members modal is open")
	// 		} else {
	// 			log.Println("first row input is visible")
	// 		}
	// 		return nil
	// 	}
	// }
	// log.Println("first row input is not visible yet")

	// if searchMore, err := page.Element(`[data-testid="search-more-members-button-top"]`); err == nil {
	// 	if visible, _ := searchMore.Visible(); visible {
	// 		log.Println("[DentaQuest] row 1 not visible; clicking Search more members button")
	// 		if err := searchMore.Click(proto.InputMouseButtonLeft, 1); err != nil {
	// 			return fmt.Errorf("click DentaQuest search more members button: %w", err)
	// 		}
	// 	}
	// }
	// log.Println("waiting for first row input")

	// _, err = page.Timeout(15 * time.Second).Element(`[data-testid="member-row-1-member-id-input"]`)
	// return err

	_, err := page.Timeout(10*time.Second).ElementR(
		`p, h1, h2, div`,
		`Search members by access point \(location and provider\)`,
	)
	if err != nil {
		return fmt.Errorf("wait for DentaQuest search members modal heading: %w", err)
	}

	time.Sleep(5 * time.Second)

	_, err = page.Timeout(10 * time.Second).Element(`[data-testid="member-row-1-member-id-input"]`)
	if err != nil {
		return fmt.Errorf("wait for DentaQuest first member row input: %w", err)
	}
	return nil
}

func ensureSearchMembersProviderSelected(page *rod.Page, providerName string) error {
	if strings.TrimSpace(providerName) == "" {
		return nil
	}

	const providerBtnSel = `button[data-testid="select-input_select-btn"][aria-label^="Provider"]`
	const providerSearchInputSel = `[data-testid="select-input_list_search-input_input"]`
	const providerOptionSel = `[data-testid="select-input_list"] li[role="option"]`

	providerBtn, err := page.Timeout(10 * time.Second).Element(providerBtnSel)
	if err != nil {
		return fmt.Errorf("find modal provider button: %w", err)
	}
	targetProvider := cleanLower(providerName)
	label, _ := providerBtn.Attribute("aria-label")
	if label != nil {
		log.Printf("[DentaQuest] modal provider label before selection=%q", *label)
		labelText := cleanLower(*label)
		if labelText != "" &&
			!strings.Contains(labelText, "select an option") &&
			!strings.Contains(labelText, "select provider") &&
			(targetProvider == "" || strings.Contains(labelText, targetProvider)) {
			log.Printf("[DentaQuest] modal provider already selected")
			return nil
		}
	}

	log.Printf("[DentaQuest] selecting modal provider %q", providerName)
	if _, err := providerBtn.Eval(`() => this.click()`); err != nil {
		return fmt.Errorf("open modal provider dropdown: %w", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ariaExpanded, _ := providerBtn.Attribute("aria-expanded")
		if ariaExpanded != nil && *ariaExpanded == "true" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	ariaExpanded, _ := providerBtn.Attribute("aria-expanded")
	if ariaExpanded == nil || *ariaExpanded != "true" {
		return fmt.Errorf("modal provider dropdown did not expand")
	}
	log.Printf("[DentaQuest] modal provider dropdown expanded")

	log.Printf("[DentaQuest] locating modal provider search input")
	searchInput, err := page.Timeout(2 * time.Second).Element(providerSearchInputSel)

	if err != nil {
		log.Printf("[DentaQuest] modal provider search input not found: %v", err)
	} else {
		log.Printf("[DentaQuest] modal provider search input found")
		if err := searchInput.Click(proto.InputMouseButtonLeft, 1); err != nil {
			log.Printf("[DentaQuest] modal provider search click failed: %v", err)
		}
		log.Printf("[DentaQuest] typing into modal provider search input")
		if err := searchInput.Input(providerName); err != nil {
			return fmt.Errorf("fill modal provider search: %w", err)
		}
		log.Printf("[DentaQuest] typed modal provider name: %q", providerName)
	}

	if options, err := page.Elements(providerOptionSel); err == nil {
		log.Printf("[DentaQuest] modal provider options currently visible=%d", len(options))
	} else {
		log.Printf("[DentaQuest] modal provider options query failed: %v", err)
	}

	option, err := page.Timeout(5 * time.Second).Element(providerOptionSel)
	if err != nil {
		return fmt.Errorf("find modal provider option for %q: %w", providerName, err)
	}
	if err := option.Click(proto.InputMouseButtonLeft, 1); err != nil {
		if _, jsErr := option.Eval(`() => this.click()`); jsErr != nil {
			return fmt.Errorf("select modal provider option: %w (fallback js click failed: %v)", err, jsErr)
		}
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ariaExpanded, _ := providerBtn.Attribute("aria-expanded")
		if ariaExpanded != nil && *ariaExpanded == "false" {
			log.Printf("[DentaQuest] modal provider selected")
			time.Sleep(200 * time.Millisecond)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("modal provider dropdown did not close after selection")
}

func ensureSearchLines(page *rod.Page, needed int) error {
	if needed <= 5 {
		log.Printf("[DentaQuest] batch size %d fits default search rows; no need to add lines", needed)
		return nil
	}

	log.Printf("[DentaQuest] batch size %d needs more rows; clicking Add 5 search lines", needed)
	addLines, err := page.Timeout(3 * time.Second).Element(`button[aria-label="Add 5 search lines"]`)
	if err != nil {
		return fmt.Errorf("find Add 5 search lines button: %w", err)
	}
	label, _ := addLines.Attribute("aria-label")
	if label != nil {
		log.Printf("[DentaQuest] addLines button label=%q", *label)
	}
	if err := addLines.ScrollIntoView(); err != nil {
		log.Printf("[DentaQuest] Add 5 search lines scroll failed: %v", err)
	}
	if err := addLines.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click DentaQuest Add 5 Search Lines: %w", err)
	}
	log.Printf("[DentaQuest] clicked Add 5 search lines")
	time.Sleep(500 * time.Millisecond)
	return nil
}

func fillMemberSearchRowAtIndex(page *rod.Page, index int, appointment models.Appointment) error {
	if strings.TrimSpace(appointment.SubscriberID) == "" || strings.TrimSpace(appointment.DOB) == "" {
		return fmt.Errorf("DentaQuest subscriberId and dob are required for row %d member search", index+1)
	}

	rowNumber := index + 1
	memberIDInput, err := page.Timeout(10 * time.Second).Element(fmt.Sprintf(`[data-testid="member-row-%d-member-id-input"]`, rowNumber))
	if err != nil {
		return fmt.Errorf("wait for DentaQuest member ID input row %d: %w", rowNumber, err)
	}
	if err := memberIDInput.Input(appointment.SubscriberID); err != nil {
		return fmt.Errorf("fill DentaQuest member ID row %d: %w", rowNumber, err)
	}

	dobGroup, err := page.Timeout(10 * time.Second).Element(fmt.Sprintf(`[data-testid="member-row-%d-date-of-birth-field"]`, rowNumber))
	if err != nil {
		return fmt.Errorf("wait for DentaQuest DOB field row %d: %w", rowNumber, err)
	}

	month, day, year, err := splitDOB(appointment.DOB)
	if err != nil {
		return fmt.Errorf("row %d: %w", rowNumber, err)
	}
	for _, seg := range []struct {
		dataType string
		value    string
	}{
		{"month", month},
		{"day", day},
		{"year", year},
	} {
		el, err := dobGroup.Element(fmt.Sprintf(`[data-type="%s"]`, seg.dataType))
		if err != nil {
			return fmt.Errorf("find DOB %s row %d: %w", seg.dataType, rowNumber, err)
		}
		if err := fillDOBSegment(page, el, seg.value); err != nil {
			return fmt.Errorf("fill DOB %s row %d: %w", seg.dataType, rowNumber, err)
		}
	}

	log.Printf(
		"[DentaQuest] filled member search row=%d patNum=%s subscriberId=%s name=%s",
		rowNumber, appointment.PatNum, appointment.SubscriberID,
		strings.TrimSpace(appointment.FName+" "+appointment.LName),
	)

	return nil
}

func fillDOBSegment(_ *rod.Page, el *rod.Element, value string) error {
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return err
	}
	// el.Input clears the field before typing, so no Ctrl+A needed.
	return el.Input(value)
}

// elementCenter returns the center point of an element via getBoundingClientRect.
func elementCenter(el *rod.Element) (proto.Point, error) {
	res, err := el.Eval(`() => {
		const r = this.getBoundingClientRect();
		return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
	}`)
	if err != nil {
		return proto.Point{}, err
	}
	return proto.Point{
		X: res.Value.Get("x").Num(),
		Y: res.Value.Get("y").Num(),
	}, nil
}

func splitDOB(value string) (string, string, string, error) {
	digits := regexp.MustCompile(`\D`).ReplaceAllString(value, "")
	if len(digits) != 8 {
		return "", "", "", fmt.Errorf("unsupported DOB format: %s", value)
	}
	if digits[:4] > "1900" {
		return digits[4:6], digits[6:8], digits[0:4], nil
	}
	return digits[0:2], digits[2:4], digits[4:8], nil
}

func cleanLower(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func waitForSearchStatuses(page *rod.Page) error {
	log.Println("Inside waitForSearchStatuses()")

	scrollSearchModalToBottom(page)
	_, err := page.Timeout(15 * time.Second).Element(`[data-testid="advanced-search-modal-add-results-button"]`)
	if err != nil {
		return fmt.Errorf("wait for DentaQuest add successful results button: %w", err)
	}
	log.Println("Completed waitForSearchStatuses()")
	return nil
}

func scrollSearchModalToBottom(page *rod.Page) {
	log.Println("Inside scrollSearchModalToBottom()")

	for _, sel := range []string{
		`section[role="dialog"][aria-label="Search members"] .overflow-y-auto.max-h-\[70vh\]`,
		`section[role="dialog"][aria-label="Search members"] .overflow-y-auto`,
		`section[role="dialog"][aria-label="Search members"]`,
	} {
		if el, err := page.Element(sel); err == nil {
			if visible, _ := el.Visible(); visible {
				_, _ = el.Eval(`() => { this.scrollTop = this.scrollHeight; }`)
				time.Sleep(500 * time.Millisecond)
				log.Printf("[DentaQuest] scrolled modal using selector=%s", sel)
				log.Println("Completed scrollSearchModalToBottom()")
				return
			}
		}
	}
	_ = page.Mouse.Scroll(0, 2000, 3)
	time.Sleep(500 * time.Millisecond)
	log.Println("Completed scrollSearchModalToBottom()")
}

func clickAddSuccessfulResults(page *rod.Page, expectedSuccessCount int) error {
	addResults, err := page.Timeout(10 * time.Second).Element(`[data-testid="advanced-search-modal-add-results-button"]`)
	if err != nil {
		return fmt.Errorf("wait for DentaQuest add results button: %w", err)
	}
	if text, textErr := addResults.Text(); textErr == nil {
		log.Printf("[DentaQuest] add successful results button text=%q", strings.TrimSpace(text))
		logging.Info("dentaquest.browser", "dentaquest.batch.add_results_button.visible", "add successful results button ready", map[string]any{
			"text": strings.TrimSpace(text),
		})
	}
	if err := addResults.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click DentaQuest add results button: %w", err)
	}
	_, err = page.Timeout(15 * time.Second).Element(`table[data-testid="member-eligibility-table_table-label"]`)
	if err != nil {
		return fmt.Errorf("wait for DentaQuest eligibility results table: %w", err)
	}
	time.Sleep(2 * time.Second)
	gridRows, rowErr := page.Elements(`table[data-testid="member-eligibility-table_table-label"] tbody tr[role="row"]`)
	if rowErr == nil {
		log.Printf("[DentaQuest] grid reconciliation: search results=%d grid rows=%d", expectedSuccessCount, len(gridRows))
		logging.Info("dentaquest.browser", "dentaquest.results_grid.reconciled", "reconciled search results with eligibility grid rows", map[string]any{
			"searchResults": expectedSuccessCount,
			"gridRows":      len(gridRows),
		})
	}
	log.Printf("[DentaQuest] eligibility results grid loaded")
	logging.Info("dentaquest.browser", "dentaquest.results_grid.loaded", "eligibility results grid loaded", nil)
	time.Sleep(1 * time.Second)
	log.Printf("[DentaQuest] debug pause at eligibility grid; resume execution from Rod pause")
	//utils.Pause()
	return nil
}

func (s *Session) OpenMemberDetailsFromSearchResults(appointment models.Appointment) error {
	page := s.browser.Page
	s.ClearPayloads()
	s.setCaptureMemberDetails(true)

	row, err := findMemberEligibilityRow(page, appointment)
	if err != nil {
		s.setCaptureMemberDetails(false)
		return err
	}

	nameButton, err := row.Element(`td:first-child button`)
	if err != nil {
		s.setCaptureMemberDetails(false)
		return fmt.Errorf("find DentaQuest member name button: %w", err)
	}
	if err := nameButton.Click(proto.InputMouseButtonLeft, 1); err != nil {
		s.setCaptureMemberDetails(false)
		return fmt.Errorf("click DentaQuest member name button: %w", err)
	}

	// Wait for URL to include member-details.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(currentURL(page), "member-details") {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !strings.Contains(currentURL(page), "member-details") {
		s.setCaptureMemberDetails(false)
		return fmt.Errorf("wait for DentaQuest member-details URL timed out")
	}

	_ = page.Timeout(5 * time.Second).WaitLoad()

	// Wait for heading and give XHR responses time to arrive.
	_, err = page.Timeout(15*time.Second).ElementR(`h1, [data-testid="member-information-header"], div.text-t-primary.text-24.font-bold`, `[Mm]ember [Ii]nformation [Ff]or`)
	if err != nil {
		s.setCaptureMemberDetails(false)
		return fmt.Errorf("wait for DentaQuest member information heading: %w", err)
	}
	s.waitForMemberDetailPayloads(12 * time.Second)
	return nil
}

func (s *Session) OpenMemberDetailsFromGridIndex(rowIndex int) error {
	page := s.browser.Page
	s.ClearPayloads()
	s.setCaptureMemberDetails(true)

	rows, err := page.Elements(`table[data-testid="member-eligibility-table_table-label"] tbody tr[role="row"]`)
	if err != nil || len(rows) == 0 {
		s.setCaptureMemberDetails(false)
		return fmt.Errorf("DentaQuest eligibility results table is empty or not loaded")
	}
	if rowIndex < 0 || rowIndex >= len(rows) {
		s.setCaptureMemberDetails(false)
		return fmt.Errorf("DentaQuest eligibility grid row index out of range: index=%d rows=%d", rowIndex, len(rows))
	}

	row := rows[rowIndex]
	if _, err := row.Eval(`() => this.scrollIntoView({block: 'center', inline: 'nearest'})`); err == nil {
		time.Sleep(400 * time.Millisecond)
	}
	nameButton, err := row.Element(`td:first-child button`)
	if err != nil {
		s.setCaptureMemberDetails(false)
		return fmt.Errorf("find DentaQuest member name button at row %d: %w", rowIndex+1, err)
	}
	if _, err := nameButton.Eval(`() => this.scrollIntoView({block: 'center', inline: 'nearest'})`); err == nil {
		time.Sleep(250 * time.Millisecond)
	}
	if err := nameButton.Click(proto.InputMouseButtonLeft, 1); err != nil {
		s.setCaptureMemberDetails(false)
		return fmt.Errorf("click DentaQuest member name button at row %d: %w", rowIndex+1, err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(currentURL(page), "member-details") {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !strings.Contains(currentURL(page), "member-details") {
		s.setCaptureMemberDetails(false)
		return fmt.Errorf("wait for DentaQuest member-details URL timed out for row %d", rowIndex+1)
	}

	_ = page.Timeout(5 * time.Second).WaitLoad()
	_, err = page.Timeout(15*time.Second).ElementR(`h1, [data-testid="member-information-header"], div.text-t-primary.text-24.font-bold`, `[Mm]ember [Ii]nformation [Ff]or`)
	if err != nil {
		s.setCaptureMemberDetails(false)
		return fmt.Errorf("wait for DentaQuest member information heading at row %d: %w", rowIndex+1, err)
	}
	s.waitForMemberDetailPayloads(12 * time.Second)
	log.Printf(
		"[DentaQuest] row=%d member details opened: payloads=%d member-eligibility=%t plan-benefit-summary=%t",
		rowIndex+1,
		s.payloadCount(),
		s.hasPayload("member-eligibility"),
		s.hasPayload("plan-benefit-summary"),
	)
	return nil
}

func (s *Session) ReturnToEligibilitySearch() error {
	page := s.browser.Page
	s.setCaptureMemberDetails(false)

	if breadcrumb, err := page.ElementR(`a`, `(?i)check eligibility.*new search`); err == nil {
		if visible, _ := breadcrumb.Visible(); visible {
			if err := breadcrumb.Click(proto.InputMouseButtonLeft, 1); err != nil {
				return fmt.Errorf("click DentaQuest eligibility breadcrumb: %w", err)
			}
		}
	} else {
		if err := page.Navigate(memberEligibilityURL); err != nil {
			return fmt.Errorf("navigate to DentaQuest eligibility search: %w", err)
		}
	}

	_, err := page.Timeout(15 * time.Second).Element(`table[data-testid="member-eligibility-table_table-label"]`)
	if err != nil {
		return fmt.Errorf("wait for DentaQuest eligibility results table after return: %w", err)
	}
	time.Sleep(1 * time.Second)
	log.Printf("[DentaQuest] returned to eligibility results grid")
	logging.Info("dentaquest.browser", "dentaquest.results_grid.returned", "returned to eligibility results grid", nil)
	return nil
}

func (s *Session) ClearAndRestartSearch() error {
	page := s.browser.Page

	clearButton, err := page.Element(`[data-testid="clear-results-button"]`)
	if err != nil {
		return nil
	}
	if visible, _ := clearButton.Visible(); !visible {
		return nil
	}
	if _, err := clearButton.Eval(`() => this.click()`); err != nil {
		return fmt.Errorf("click DentaQuest clear results button: %w", err)
	}

	confirmButton, err := page.Timeout(10 * time.Second).Element(`[data-testid="modal-submit-button"]`)
	if err != nil {
		return fmt.Errorf("wait for DentaQuest clear results confirm modal: %w", err)
	}
	if _, err := confirmButton.Eval(`() => this.click()`); err != nil {
		return fmt.Errorf("confirm DentaQuest clear results: %w", err)
	}

	_, err = page.Timeout(15 * time.Second).Element(`[data-testid="member-row-1-member-id-input"]`)
	if err != nil {
		return fmt.Errorf("wait for DentaQuest member search form after clear: %w", err)
	}
	log.Printf("[DentaQuest] results cleared; member search form ready for next batch")
	logging.Info("dentaquest.browser", "dentaquest.batch.results_cleared", "results cleared and member search form ready", nil)
	return nil
}

func findMemberEligibilityRow(page *rod.Page, appointment models.Appointment) (*rod.Element, error) {
	rows, err := page.Elements(`table[data-testid="member-eligibility-table_table-label"] tbody tr[role="row"]`)
	if err != nil || len(rows) == 0 {
		return nil, fmt.Errorf("DentaQuest eligibility results table is empty or not loaded")
	}

	targetID := normalizeDigits(appointment.SubscriberID)
	targetDOB := normalizeDateDigits(appointment.DOB)
	targetFirst := firstNameToken(appointment.FName)
	targetLast := lastNameToken(appointment.LName)

	for _, row := range rows {
		cells, err := row.Elements("td")
		if err != nil || len(cells) < 2 {
			continue
		}

		memberIDText, _ := cells[1].Text()
		rowID := normalizeDigits(memberIDText)

		var rowName string
		if btn, err := cells[0].Element("button"); err == nil {
			label, _ := btn.Attribute("aria-label")
			if label != nil {
				rowName = cleanLower(*label)
			}
		}
		if rowName == "" {
			text, _ := cells[0].Text()
			rowName = cleanLower(text)
		}

		firstText, _ := cells[0].Text()
		rowDOB := ""
		if m := regexp.MustCompile(`(?i)dob:\s*([0-9/.\-]+)`).FindStringSubmatch(firstText); len(m) > 1 {
			rowDOB = normalizeDateDigits(m[1])
		}

		if targetDOB != "" && rowDOB != "" && targetDOB != rowDOB {
			continue
		}

		if rowMatchesAppointment(rowName, targetFirst, targetLast) {
			return row, nil
		}

		if targetID != "" && rowID == targetID {
			return row, nil
		}
	}

	return nil, fmt.Errorf(
		"DentaQuest eligibility row not found: patNum=%s subscriberId=%s name=%s",
		appointment.PatNum, appointment.SubscriberID, cleanLower(strings.TrimSpace(appointment.FName+" "+appointment.LName)),
	)
}

func rowMatchesAppointment(rowName, targetFirst, targetLast string) bool {
	if rowName == "" {
		return false
	}

	rowTokens := strings.Fields(rowName)
	if len(rowTokens) == 0 {
		return false
	}

	if targetLast != "" && !tokenListContains(rowTokens, targetLast) {
		return false
	}

	if targetFirst == "" {
		return true
	}

	for _, token := range rowTokens {
		if token == targetFirst || strings.HasPrefix(token, targetFirst) || strings.HasPrefix(targetFirst, token) {
			return true
		}
	}
	return false
}

func tokenListContains(tokens []string, target string) bool {
	for _, token := range tokens {
		if token == target {
			return true
		}
	}
	return false
}

func firstNameToken(value string) string {
	tokens := strings.Fields(cleanLower(value))
	if len(tokens) == 0 {
		return ""
	}
	return tokens[0]
}

func lastNameToken(value string) string {
	tokens := strings.Fields(cleanLower(value))
	if len(tokens) == 0 {
		return ""
	}
	return tokens[len(tokens)-1]
}

func inspectSearchStatuses(page *rod.Page, batch []models.Appointment) *SearchSummary {
	log.Println("Inside inspectSearchStatuses")

	summary := &SearchSummary{BatchSize: len(batch)}
	for index := range batch {
		rowNumber := index + 1
		log.Printf("[DentaQuest] checking status for row %d", rowNumber)
		statusSel := fmt.Sprintf(`button[data-testid^="member-row-%d-"][data-testid$="-status"]`, rowNumber)
		if statusEl, err := page.Timeout(1 * time.Second).Element(statusSel); err == nil {
			label, _ := statusEl.Attribute("aria-label")
			if label != nil {
				normalized := cleanLower(*label)
				log.Printf("[DentaQuest] row %d status label=%q", rowNumber, *label)
				switch {
				case strings.Contains(normalized, "search status: error"):
					log.Printf("[DentaQuest] row %d status=error", rowNumber)
					summary.Errored++
				case strings.Contains(normalized, "member not found"):
					log.Printf("[DentaQuest] row %d status=not found", rowNumber)
					summary.NotFound++
				case strings.Contains(normalized, "member found"):
					log.Printf("[DentaQuest] row %d status=found", rowNumber)
					summary.SuccessCount++
					summary.SuccessfulAppointments = append(summary.SuccessfulAppointments, batch[index])
				default:
					log.Printf("[DentaQuest] row %d status=unclassified", rowNumber)
				}
			}
		} else {
			log.Printf("[DentaQuest] row %d status element not found: %v", rowNumber, err)
		}
	}

	log.Printf("[DentaQuest] summary before add-results fallback: success=%d notFound=%d error=%d", summary.SuccessCount, summary.NotFound, summary.Errored)
	if summary.SuccessCount == 0 {
		if addResults, err := page.Element(`[data-testid="advanced-search-modal-add-results-button"]`); err == nil {
			text, _ := addResults.Text()
			log.Printf("[DentaQuest] add-results button text=%q", text)
			if match := regexp.MustCompile(`(?i)add\s+(\d+)\s+successful`).FindStringSubmatch(text); len(match) == 2 {
				_, _ = fmt.Sscanf(match[1], "%d", &summary.SuccessCount)
				if len(summary.SuccessfulAppointments) == 0 && summary.SuccessCount <= len(batch) {
					summary.SuccessfulAppointments = append(summary.SuccessfulAppointments, batch[:summary.SuccessCount]...)
				}
			}
		} else {
			log.Printf("[DentaQuest] add-results button not found during fallback: %v", err)
		}
	}
	log.Println("Completed inspectSearchStatuses")
	return summary
}

func waitForAddResultsButtonCount(page *rod.Page, expectedCount int, timeout time.Duration) error {
	if page == nil {
		return fmt.Errorf("page is nil")
	}
	if expectedCount < 0 {
		return fmt.Errorf("invalid expected count")
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		addResults, err := page.Element(`[data-testid="advanced-search-modal-add-results-button"]`)
		if err == nil {
			if text, textErr := addResults.Text(); textErr == nil {
				if actualCount, ok := parseAddResultsButtonCount(text); ok {
					if actualCount == expectedCount {
						log.Printf("[DentaQuest] add-results button count settled: expected=%d actual=%d", expectedCount, actualCount)
						return nil
					}
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	addResults, err := page.Element(`[data-testid="advanced-search-modal-add-results-button"]`)
	if err != nil {
		return fmt.Errorf("button not found")
	}
	text, _ := addResults.Text()
	actualCount, _ := parseAddResultsButtonCount(text)
	return fmt.Errorf("expected=%d actual=%d text=%q", expectedCount, actualCount, strings.TrimSpace(text))
}

func parseAddResultsButtonCount(text string) (int, bool) {
	match := regexp.MustCompile(`(?i)add\s+(\d+)\s+successful`).FindStringSubmatch(text)
	if len(match) != 2 {
		return 0, false
	}
	var count int
	if _, err := fmt.Sscanf(match[1], "%d", &count); err != nil {
		return 0, false
	}
	return count, true
}
