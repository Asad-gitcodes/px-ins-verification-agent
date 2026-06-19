package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/cache"
	"insurance-benefit-agent-go/internal/config"
	"insurance-benefit-agent-go/internal/controlplane"
	"insurance-benefit-agent-go/internal/jobmgr"
	"insurance-benefit-agent-go/internal/mfa"
	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
	dqapi "insurance-benefit-agent-go/internal/payers/dentaquest/api"
	dqbrowser "insurance-benefit-agent-go/internal/payers/dentaquest/browser"
)

func main() {
	flagOfficeKey   := flag.String("office-key", "", "Office key (required)")
	flagPatconURL   := flag.String("patcon-url", "", "Patcon bootstrap URL (required)")
	flagPatconToken := flag.String("patcon-token", "", "Patcon bearer token (required)")
	appointmentsPath := flag.String("appointments", "", "Path to appointments JSON array")
	selectProvider := flag.Bool("select-provider", false, "Select dashboard provider before probing APIs")
	limit := flag.Int("limit", 0, "Optional max number of appointments to probe")
	flag.Parse()

	var missing []string
	if *flagOfficeKey == "" {
		missing = append(missing, "--office-key")
	}
	if *flagPatconURL == "" {
		missing = append(missing, "--patcon-url")
	}
	if *flagPatconToken == "" {
		missing = append(missing, "--patcon-token")
	}
	if strings.TrimSpace(*appointmentsPath) == "" {
		missing = append(missing, "--appointments")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "error: required flags missing: %s\n\n", strings.Join(missing, ", "))
		flag.Usage()
		os.Exit(1)
	}

	cfg, err := config.Default("")
	if err != nil {
		log.Fatalf("default config: %v", err)
	}
	cfg.OfficeKey = *flagOfficeKey
	cfg.Bootstrap.Patcon.URL = *flagPatconURL
	cfg.Bootstrap.Patcon.Token = *flagPatconToken

	bgCtx := context.Background()
	serverConfig, err := controlplane.NewClient(cfg).FetchServerConfig(bgCtx)
	if err != nil {
		log.Fatalf("fetch server config: %v", err)
	}
	snapshot, err := jobmgr.MapServerConfig(cfg, serverConfig)
	if err != nil {
		log.Fatalf("map server config: %v", err)
	}

	payer, credential, err := dentaQuestPayerAndCredential(snapshot)
	if err != nil {
		log.Fatalf("resolve DentaQuest payer/credential: %v", err)
	}

	emailMFA, err := buildEmailMFA(snapshot, credential)
	if err != nil {
		log.Fatalf("build MFA config: %v", err)
	}

	appointments, err := loadAppointments(*appointmentsPath)
	if err != nil {
		log.Fatalf("load appointments: %v", err)
	}
	if *limit > 0 && *limit < len(appointments) {
		appointments = appointments[:*limit]
	}
	if len(appointments) == 0 {
		log.Fatal("no appointments to probe")
	}

	input := payers.SessionInput{
		Payer:              payer,
		Credential:         credential,
		Password:           credential.Password,
		EmailMFA:           emailMFA,
		Appointments:       appointments,
		ScraperConfig:      snapshot.ScraperConfig,
		RequestedOfficeKey: snapshot.OfficeKey,
		Testing:            cfg.Testing,
	}

	session, err := dqbrowser.Launch(input)
	if err != nil {
		log.Fatalf("launch DentaQuest browser: %v", err)
	}
	defer session.Close()

	if err := session.Login(input); err != nil {
		log.Fatalf("login DentaQuest: %v", err)
	}
	if *selectProvider {
		if err := dqbrowser.SelectDashboardProvider(session.Page(), credential.ProviderName); err != nil {
			log.Fatalf("select member-search home provider: %v", err)
		}
	}

	probe := dqapi.NewBrowserProbe(session)
	dateOfService := normalizeAPIDate(appointments[0].AppointmentDate)
	log.Printf("[DentaQuest API Probe] run starting appointments=%d dateOfService=%s appointmentsFile=%s", len(appointments), dateOfService, *appointmentsPath)
	ctx, err := probe.DiscoverPracticeContext(dateOfService, credential.ProviderName)
	if err != nil {
		log.Fatalf("discover practice context: %v", err)
	}
	log.Printf("practice context: location=%s practitioner=%s accessPoint=%s routeId=%s", ctx.ServiceLocation, ctx.PractitionerName, ctx.AccessPointID, ctx.RouteID)

	runStamp := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	outputDir := filepath.Join("artifacts", snapshot.OfficeKey, runStamp, "DentaQuest.api-probe")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	for _, appointment := range appointments {
		log.Printf("[DentaQuest API Probe] searching patNum=%s aptNum=%s subscriberId=%s name=%s %s dob=%s appointmentDate=%s",
			appointment.PatNum,
			appointment.AptNum,
			appointment.SubscriberID,
			strings.TrimSpace(appointment.FName),
			strings.TrimSpace(appointment.LName),
			strings.TrimSpace(appointment.DOB),
			strings.TrimSpace(appointment.AppointmentDate),
		)
		bundle, err := probe.SearchAndFetchPatient(*ctx, appointment)
		if err != nil {
			log.Printf("[DentaQuest API Probe] patNum=%s subscriberId=%s failed: %v", appointment.PatNum, appointment.SubscriberID, err)
			continue
		}
		filePath := filepath.Join(outputDir, fmt.Sprintf("%s_%s_api_probe.json", sanitize(appointment.PatNum), sanitize(appointment.AptNum)))
		raw, err := json.MarshalIndent(bundle, "", "  ")
		if err != nil {
			log.Printf("[DentaQuest API Probe] marshal patNum=%s failed: %v", appointment.PatNum, err)
			continue
		}
		if err := os.WriteFile(filePath, raw, 0o644); err != nil {
			log.Printf("[DentaQuest API Probe] write patNum=%s failed: %v", appointment.PatNum, err)
			continue
		}
		log.Printf("[DentaQuest API Probe] wrote %s", filePath)
	}

	_ = context.Background()
}

func dentaQuestPayerAndCredential(snapshot *cache.WorkSnapshot) (models.Payer, models.CredentialCandidate, error) {
	for _, payer := range snapshot.Payers {
		if strings.EqualFold(payer.PayerURL, "DentaQuest.com") {
			return payer, payer.Credential, nil
		}
	}
	return models.Payer{}, models.CredentialCandidate{}, fmt.Errorf("DentaQuest payer not found in snapshot")
}

func buildEmailMFA(snapshot *cache.WorkSnapshot, credential models.CredentialCandidate) (*mfa.EmailConfig, error) {
	if !strings.EqualFold(credential.MFAMethod, "email") || snapshot == nil || snapshot.ScraperConfig == nil {
		return nil, nil
	}
	cfg := snapshot.ScraperConfig.MFA.Email
	if cfg.Password == "" {
		return nil, fmt.Errorf("email MFA password is missing")
	}
	return &mfa.EmailConfig{
		Host:            cfg.Host,
		Port:            cfg.Port,
		Secure:          cfg.Secure,
		User:            cfg.User,
		Password:        cfg.Password,
		ExpectedTo:      expectedMFAToAddress(cfg.User, credential.Username),
		DeleteAfterRead: true,
		CleanupMailbox:  "[Gmail]/Trash",
		Mailbox:         cfg.Mailbox,
		TimeoutMS:       cfg.TimeoutMS,
		PollIntervalMS:  cfg.PollIntervalMS,
	}, nil
}

func expectedMFAToAddress(mailboxUser string, credentialUsername string) string {
	credentialUsername = strings.TrimSpace(credentialUsername)
	if credentialUsername == "" || !strings.Contains(mailboxUser, "@") {
		return ""
	}
	localPart, domain, _ := strings.Cut(mailboxUser, "@")
	if strings.Contains(credentialUsername, "@") {
		return strings.ToLower(credentialUsername)
	}
	if strings.Contains(credentialUsername, "+") {
		return strings.ToLower(credentialUsername + "@" + domain)
	}
	if !strings.HasPrefix(strings.ToLower(credentialUsername), strings.ToLower(localPart)) {
		return ""
	}
	suffix := credentialUsername[len(localPart):]
	accountEnd := 0
	for accountEnd < len(suffix) && suffix[accountEnd] >= '0' && suffix[accountEnd] <= '9' {
		accountEnd++
	}
	if accountEnd == 0 || accountEnd == len(suffix) {
		return ""
	}
	account := suffix[:accountEnd]
	payerName := suffix[accountEnd:]
	return strings.ToLower(fmt.Sprintf("%s+%s+%s@%s", localPart, account, payerName, domain))
}

func loadAppointments(path string) ([]models.Appointment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var appointments []models.Appointment
	if err := json.Unmarshal(raw, &appointments); err != nil {
		return nil, err
	}
	return appointments, nil
}

func normalizeAPIDate(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Now().UTC().Format("2006-01-02")
	}
	layouts := []string{"2006-01-02", "01-02-2006", "01/02/2006", "1/2/2006"}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Format("2006-01-02")
		}
	}
	return value
}

func sanitize(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	replacer := strings.NewReplacer("\\", "_", "/", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	return replacer.Replace(value)
}
