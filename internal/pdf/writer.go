package pdf

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/advanced"
	"insurance-benefit-agent-go/internal/browser"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

//go:embed summary.html
var summaryTemplateHTML string

var globalPDFBrowser *rod.Browser
var globalPDFLauncher *launcher.Launcher
var globalPDFTempDir string
var globalPDFPID int

// Writer renders eligibility reports to PDF using a dedicated headless Chrome instance.
// The headless browser is launched lazily on first use and reused across calls.
type Writer struct{}

func NewWriter() *Writer {
	return &Writer{}
}

func getPDFBrowser() (*rod.Browser, error) {
	// Probe the existing browser with a cheap ping; reset if it's dead.
	if globalPDFBrowser != nil {
		if _, err := globalPDFBrowser.Version(); err != nil {
			log.Printf("[pdf] headless browser went away (%v), relaunching", err)
			if globalPDFLauncher != nil {
				globalPDFLauncher.Kill()
			}
			if globalPDFTempDir != "" {
				_ = os.RemoveAll(globalPDFTempDir)
			}
			globalPDFBrowser = nil
			globalPDFLauncher = nil
			globalPDFTempDir = ""
		}
	}

	if globalPDFBrowser != nil {
		return globalPDFBrowser, nil
	}

	// Use a per-run temp dir to avoid Chrome singleton lock conflicts.
	dir, err := os.MkdirTemp("", "agent-pdf-chrome-*")
	if err != nil {
		return nil, fmt.Errorf("create pdf chrome temp dir: %w", err)
	}

	l := browser.ConfigureLauncher(launcher.New()).
		UserDataDir(dir).
		Leakless(false). // leakless.exe is flagged by Windows Defender
		HeadlessNew(true)

	log.Printf("[pdf] launching headless Chrome tempDir=%s", dir)
	u, err := l.Launch()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("launch headless chrome for pdfs: %w", err)
	}
	pid := l.PID()
	log.Printf("[pdf] headless Chrome launched pid=%d", pid)

	b := rod.New().ControlURL(u)
	if err := b.Connect(); err != nil {
		browser.ForceKillProcessTree(pid)
		l.Kill()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("connect to headless chrome for pdfs: %w", err)
	}
	log.Printf("[pdf] headless Chrome connected pid=%d", pid)

	globalPDFBrowser = b
	globalPDFLauncher = l
	globalPDFTempDir = dir
	globalPDFPID = pid
	return globalPDFBrowser, nil
}

// CloseBrowser tears down the shared headless Chrome used for PDF rendering.
// Payer sessions call this between payers so no Chrome process survives into
// the next payer login flow.
func CloseBrowser() error {
	b := globalPDFBrowser
	launcherRef := globalPDFLauncher
	tempDir := globalPDFTempDir
	pid := globalPDFPID
	globalPDFBrowser = nil
	globalPDFLauncher = nil
	globalPDFTempDir = ""
	globalPDFPID = 0

	if b == nil {
		return nil
	}

	log.Printf("[pdf] closing headless Chrome pid=%d", pid)

	// Mirror the scraping browser close order: forceKill → launcher.Kill → browser.Close → RemoveAll.
	browser.ForceKillProcessTree(pid)
	if launcherRef != nil {
		launcherRef.Kill()
	}

	var err error
	done := make(chan error, 1)
	go func() { done <- b.Close() }()
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		err = fmt.Errorf("pdf browser close timed out")
	}

	if tempDir != "" {
		_ = os.RemoveAll(tempDir)
	}
	if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
		return nil
	}
	log.Printf("[pdf] headless Chrome closed pid=%d", pid)
	return err
}

func (w *Writer) WriteEligibilityPDF(report *advanced.PatientEligibilityReport) ([]byte, error) {
	if report == nil {
		return nil, fmt.Errorf("advanced report is nil")
	}

	funcMap := template.FuncMap{
		"contains": strings.Contains,
		"lower":    strings.ToLower,
		"dollar": func(f float64) string {
			return fmt.Sprintf("$%.2f", f)
		},
		"pct": func(used, total float64) string {
			if total <= 0 {
				return "0"
			}
			v := used / total * 100
			if v > 100 {
				v = 100
			}
			return fmt.Sprintf("%.1f", v)
		},
		"covPct": func(pct int) string {
			if pct < 0 {
				return "–"
			}
			return fmt.Sprintf("%d%%", pct)
		},
		"statusClass": func(label string) string {
			switch strings.ToLower(strings.TrimSpace(label)) {
			case "active":
				return "active"
			case "not found":
				return "notfound"
			default:
				return "inactive"
			}
		},
		"riskClass": func(level string) string {
			switch strings.ToUpper(strings.TrimSpace(level)) {
			case "ACTIVE":
				return "eligible"
			case "RISKY":
				return "risky"
			case "UNKNOWN":
				return "unknown"
			default:
				return "denied"
			}
		},
		"riskIcon": func(level string) string {
			switch strings.ToUpper(strings.TrimSpace(level)) {
			case "ACTIVE":
				return "✓"
			case "RISKY":
				return "⚠"
			default:
				return "✗"
			}
		},
		"riskLabel": func(level string) string {
			switch strings.ToUpper(strings.TrimSpace(level)) {
			case "ACTIVE":
				return "Eligible"
			case "RISKY":
				return "Risky"
			case "UNKNOWN":
				return "Unknown"
			default:
				return "Denied"
			}
		},
		"yesNo": func(b bool) string {
			if b {
				return "Yes"
			}
			return "No"
		},
		"short": func(value string, max int) string {
			value = strings.Join(strings.Fields(value), " ")
			if max <= 0 || len(value) <= max {
				return value
			}
			if max <= 3 {
				return value[:max]
			}
			return strings.TrimSpace(value[:max-3]) + "..."
		},
		"join": func(values []string, sep string) string {
			return strings.Join(values, sep)
		},
		"sourceLabel":         sourceLabel,
		"displayCodes":        displayCodes,
		"displayAccumulators": displayAccumulators,
		"noDisplayAccumulators": func(report *advanced.PatientEligibilityReport) bool {
			if report == nil {
				return true
			}
			return len(displayAccumulators(report.Maximums)) == 0 && len(displayAccumulators(report.Deductibles)) == 0
		},
		"sparseReport": sparseReport,
		"basicReport": func(r *advanced.PatientEligibilityReport) bool {
			if r == nil || r.StatusOnly {
				return false
			}
			return len(r.Codes) == 0 &&
				len(r.Matrix) == 0 &&
				len(r.MatrixColumns) == 0 &&
				len(r.Maximums) == 0 &&
				len(r.Deductibles) == 0
		},
	}
	tmpl, err := template.New("summary").Funcs(funcMap).Parse(summaryTemplateHTML)
	if err != nil {
		return nil, fmt.Errorf("parse pdf template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, report); err != nil {
		return nil, fmt.Errorf("execute pdf template: %w", err)
	}

	browser, err := getPDFBrowser()
	if err != nil {
		return nil, err
	}

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, fmt.Errorf("open pdf page: %w", err)
	}
	defer page.Close()

	if err := page.SetDocumentContent(buf.String()); err != nil {
		return nil, fmt.Errorf("set document content: %w", err)
	}
	// Wait for the page to finish rendering before printing.
	// SetDocumentContent starts the render pipeline but returns immediately;
	// without this wait the PDF can come out blank or partially rendered.
	_ = page.Timeout(15 * time.Second).WaitLoad()

	pdfStream, err := page.PDF(&proto.PagePrintToPDF{
		PrintBackground: true,
	})
	if err != nil {
		return nil, fmt.Errorf("generate pdf from page: %w", err)
	}

	pdfBytes, err := io.ReadAll(pdfStream)
	if err != nil {
		return nil, fmt.Errorf("read pdf stream: %w", err)
	}

	return pdfBytes, nil
}

func displayCodes(codes []advanced.AdvancedCode) []advanced.AdvancedCode {
	out := make([]advanced.AdvancedCode, 0, len(codes))
	hasTP := false
	for _, code := range codes {
		if code.TP {
			hasTP = true
			break
		}
	}
	for _, code := range codes {
		if hasTP && !code.TP {
			continue
		}
		if displayableCode(code) {
			out = append(out, code)
		}
	}
	return out
}

func displayableCode(code advanced.AdvancedCode) bool {
	return code.CoveragePercent >= 0 || code.NotCovered || strings.ToUpper(strings.TrimSpace(code.Risk.Level)) != "UNKNOWN"
}

func sourceLabel(source string) string {
	switch strings.TrimSpace(source) {
	case "DentalXChangeClaimConnect":
		return "DentalXChange ClaimConnect"
	case "DeltaDentalAPIProbe":
		return "Delta Dental direct payer API"
	case "UHCDentalAPIProbe":
		return "UnitedHealthcare Dental direct payer API"
	case "":
		return ""
	default:
		return source
	}
}

func displayAccumulators(accums []advanced.AccumulatorSummary) []advanced.AccumulatorSummary {
	out := make([]advanced.AccumulatorSummary, 0, len(accums))
	byKey := map[string]int{}
	for _, acc := range accums {
		if acc.Amount <= 0 && acc.Remaining <= 0 {
			continue
		}
		key := accumulatorDisplayKey(acc)
		if idx, ok := byKey[key]; ok {
			out[idx].Services = appendUnique(out[idx].Services, accumulatorService(acc))
			if out[idx].Note == "" && acc.Note != "" {
				out[idx].Note = acc.Note
			}
			continue
		}
		acc.Services = appendUnique(acc.Services, accumulatorService(acc))
		byKey[key] = len(out)
		out = append(out, acc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return accumulatorPriority(out[i]) < accumulatorPriority(out[j])
	})
	// Drop OON entries whose amounts are identical to a matching in-network entry
	// (same kind/type/scope/amount/used/remaining) — they represent the same pool.
	inNetworkAmountKey := func(acc advanced.AccumulatorSummary) string {
		return strings.Join([]string{
			strings.ToLower(acc.Kind),
			strings.ToLower(acc.Type),
			strings.ToLower(acc.Scope),
			fmt.Sprintf("%.2f|%.2f|%.2f", acc.Amount, acc.Used, acc.Remaining),
		}, "|")
	}
	inKeys := map[string]bool{}
	for _, acc := range out {
		if accumulatorNetwork(acc.Name) == "in" {
			inKeys[inNetworkAmountKey(acc)] = true
		}
	}
	filtered := out[:0]
	for _, acc := range out {
		if accumulatorNetwork(acc.Name) == "out" && inKeys[inNetworkAmountKey(acc)] {
			continue
		}
		filtered = append(filtered, acc)
	}
	out = filtered
	for i := range out {
		if strings.EqualFold(out[i].Kind, "maximum") && len(out[i].Services) > 1 {
			out[i].Name = accumulatorBaseName(out[i])
		}
	}
	return out
}

func accumulatorDisplayKey(acc advanced.AccumulatorSummary) string {
	parts := []string{
		strings.ToLower(acc.Kind),
		strings.ToLower(acc.Type),
		strings.ToLower(acc.Scope),
		accumulatorNetwork(acc.Name),
		fmt.Sprintf("%.2f", acc.Amount),
		fmt.Sprintf("%.2f", acc.Used),
		fmt.Sprintf("%.2f", acc.Remaining),
	}
	if !strings.EqualFold(acc.Kind, "maximum") {
		parts = append(parts, strings.ToLower(strings.TrimSpace(acc.Name)))
	}
	return strings.Join(parts, "|")
}

func accumulatorPriority(acc advanced.AccumulatorSummary) int {
	score := 100
	if strings.EqualFold(acc.Kind, "maximum") {
		score -= 50
	}
	if strings.EqualFold(acc.Scope, "individual") {
		score -= 20
	}
	if strings.EqualFold(acc.Type, "calendar") {
		score -= 10
	}
	if accumulatorService(acc) == "" {
		score -= 8
	}
	if accumulatorNetwork(acc.Name) == "in" {
		score -= 5
	}
	return score
}

func accumulatorNetwork(name string) string {
	if strings.Contains(strings.ToLower(name), "oon") || strings.Contains(strings.ToLower(name), "out-of-network") {
		return "out"
	}
	return "in"
}

func accumulatorBaseName(acc advanced.AccumulatorSummary) string {
	parts := []string{}
	if strings.TrimSpace(acc.Scope) != "" {
		parts = append(parts, strings.Title(strings.ToLower(acc.Scope)))
	}
	if strings.TrimSpace(acc.Type) != "" {
		parts = append(parts, strings.Title(strings.ToLower(acc.Type)))
	}
	parts = append(parts, strings.Title(strings.ToLower(acc.Kind)))
	if accumulatorNetwork(acc.Name) == "out" {
		parts = append(parts, "(OON)")
	}
	return strings.Join(parts, " ")
}

func accumulatorService(acc advanced.AccumulatorSummary) string {
	name := strings.TrimSpace(acc.Name)
	replacers := []string{
		"Individual", "Family", "Calendar", "Lifetime", "Maximum", "Deductible",
		"(OON)", "OON", "Out-of-Network", "In-Network",
	}
	for _, part := range replacers {
		name = strings.ReplaceAll(name, part, "")
	}
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || strings.EqualFold(name, acc.Scope) || strings.EqualFold(name, acc.Type) {
		return ""
	}
	return name
}

func appendUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if strings.EqualFold(existing, value) {
			return values
		}
	}
	return append(values, value)
}

func sparseReport(report *advanced.PatientEligibilityReport) bool {
	if report == nil || report.StatusOnly {
		return false
	}
	return len(displayAccumulators(report.Maximums)) == 0 &&
		len(displayAccumulators(report.Deductibles)) == 0 &&
		len(displayCodes(report.Codes)) == 0
}
