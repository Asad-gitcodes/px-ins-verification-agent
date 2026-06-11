// cmd/ddtest — standalone Delta Dental WA end-to-end test runner.
//
// Pulls real appointments directly from query.8px.us (bypasses PatCon),
// then runs the full Delta Dental adapter: login → member search → benefits.
//
// Usage:
//
//	go run ./cmd/ddtest \
//	  --username "YOUR_DD_USERNAME" \
//	  --password "YOUR_DD_PASSWORD" \
//	  --query-token "3edfgbhnjkuyt" \
//	  --query-key   "OWU1MTA0Njc5MTJlOTkxN2UyMWIxMWM0ZjVmYjZl" \
//	  --office-key  "laguna" \
//	  --headless=false
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"insurance-benefit-agent-go/internal/models"
	"insurance-benefit-agent-go/internal/payers"
	ddapi "insurance-benefit-agent-go/internal/payers/deltadentalins/api"
	ddbrowser "insurance-benefit-agent-go/internal/payers/deltadentalins/browser"
)

const appointmentsQuery = `SELECT
  a.AptNum AS aptNum,
  DATE_FORMAT(a.AptDateTime,'%m-%d-%Y') AS appointmentDate,
  a.PatNum AS patNum,
  p.FName AS fName,
  p.LName AS lName,
  DATE_FORMAT(p.Birthdate,'%m-%d-%Y') AS dob,
  sub_p.FName AS subFName,
  sub_p.LName AS subLName,
  DATE_FORMAT(sub_p.Birthdate,'%m-%d-%Y') AS subDOB,
  ins.SubscriberID AS subscriberId,
  ip.GroupNum AS groupNum,
  ip.GroupName AS groupName,
  c.ElectID AS payerId,
  pp.Relationship AS relationship,
  GROUP_CONCAT(DISTINCT pc.ProcCode ORDER BY pc.ProcCode SEPARATOR ', ') AS treatmentPlanProcCodes
FROM appointment a
JOIN patient p ON p.PatNum=a.PatNum
JOIN patplan pp ON pp.PatNum=a.PatNum AND pp.Ordinal=1
JOIN inssub ins ON ins.InsSubNum=pp.InsSubNum
JOIN patient sub_p ON sub_p.PatNum=ins.Subscriber
JOIN insplan ip ON ip.PlanNum=ins.PlanNum
JOIN carrier c ON c.CarrierNum=ip.CarrierNum AND c.ElectID IN ('91062')
LEFT JOIN procedurelog pl ON pl.PatNum=a.PatNum AND pl.ProcStatus='1'
LEFT JOIN procedurecode pc ON pc.CodeNum=pl.CodeNum AND pc.ProcCode REGEXP '^D[0-9]+$'
WHERE a.AptStatus=1
  AND DATE(a.AptDateTime) BETWEEN CURRENT_DATE+INTERVAL 0 DAY AND CURRENT_DATE+INTERVAL 90 DAY
GROUP BY a.AptNum,a.AptDateTime,a.PatNum,p.FName,p.LName,p.Birthdate,
         sub_p.FName,sub_p.LName,sub_p.Birthdate,ins.SubscriberID,
         ip.GroupNum,ip.GroupName,c.ElectID,pp.Relationship
ORDER BY a.AptDateTime ASC
LIMIT 20`

func main() {
	ddUser    := flag.String("username",    "", "Delta Dental portal username (required)")
	ddPass    := flag.String("password",    "", "Delta Dental portal password (required)")
	qToken    := flag.String("query-token", "3edfgbhnjkuyt", "query.8px.us Authorization header")
	qKey      := flag.String("query-key",   "OWU1MTA0Njc5MTJlOTkxN2UyMWIxMWM0ZjVmYjZl", "query.8px.us key")
	officeKey := flag.String("office-key",  "laguna", "Office key used for cache/artifact paths")
	headless  := flag.Bool("headless",      true, "Run browser headless (set false to watch it)")
	mfaMethod := flag.String("mfa",         "sms", "MFA method: sms or email")
	flag.Parse()

	if *ddUser == "" || *ddPass == "" {
		fmt.Fprintln(os.Stderr, "error: --username and --password are required")
		flag.Usage()
		os.Exit(1)
	}

	log.SetFlags(log.Ltime)
	log.SetOutput(os.Stdout)

	// ── Step 1: fetch appointments from query.8px.us ──────────────────────────
	log.Printf("fetching Delta Dental appointments from query.8px.us ...")
	appointments, err := fetchAppointments(*qToken, *qKey)
	if err != nil {
		log.Fatalf("fetch appointments: %v", err)
	}
	if len(appointments) == 0 {
		log.Fatalf("no Delta Dental (ElectID=91062) appointments found in next 90 days")
	}
	log.Printf("found %d appointments:", len(appointments))
	for _, a := range appointments {
		log.Printf("  aptNum=%-8s patNum=%-6s %-20s dob=%s subscriberId=%s",
			a.AptNum, a.PatNum, a.FName+" "+a.LName, a.DOB, a.SubscriberID)
	}

	// ── Step 2: build SessionInput ────────────────────────────────────────────
	input := payers.SessionInput{
		Payer: models.Payer{PayerURL: "DeltaDentalIns.com"},
		Credential: models.CredentialCandidate{
			Username:  *ddUser,
			Password:  *ddPass,
			MFAMethod: *mfaMethod,
		},
		Password:           *ddPass,
		RequestedOfficeKey: *officeKey,
		Headless:           *headless,
		Appointments:       appointments,
		ScraperConfig: &models.ScraperConfig{
			APIs: map[string]any{
				"query": map[string]any{
					"url":   "https://query.8px.us/api/run/query",
					"token": *qToken,
					"key":   *qKey,
				},
			},
		},
	}

	// ── Step 3: launch browser & login ───────────────────────────────────────
	log.Printf("launching Delta Dental browser (headless=%v) ...", *headless)
	session, err := ddbrowser.Launch(input)
	if err != nil {
		log.Fatalf("browser launch: %v", err)
	}
	defer session.Close()

	log.Printf("logging in ...")
	if err := session.Login(input); err != nil {
		log.Fatalf("login: %v", err)
	}
	log.Printf("login OK")

	// ── Step 4: run probe for each appointment ────────────────────────────────
	outDir := filepath.Join("artifacts", "ddtest", *officeKey, time.Now().UTC().Format("2006-01-02T15-04-05Z"))
	_ = os.MkdirAll(outDir, 0o755)

	hashCache := ddapi.LoadHashCache(*officeKey)
	probe := ddapi.NewBrowserProbe(session, hashCache)

	ctx := context.Background()
	_ = ctx

	for _, appt := range appointments {
		log.Printf("── probing patNum=%s %s %s ──", appt.PatNum, appt.FName, appt.LName)
		bundle, err := probe.SearchAndFetchPatient(appt)
		if err != nil {
			log.Printf("  ERROR: %v", err)
			continue
		}
		if bundle.NotFound {
			log.Printf("  NOT FOUND on portal")
			continue
		}
		if bundle.Inactive {
			log.Printf("  INACTIVE member")
			continue
		}

		// Print summary
		if bundle.MemberSearch != nil {
			ms := bundle.MemberSearch
			log.Printf("  member:  %s %s", ms.SubscriberFirstName, ms.SubscriberLastName)
			log.Printf("  company: %s", ms.MemberCompanyName)
			log.Printf("  group:   %s", ms.GroupName)
			log.Printf("  active:  %v", ms.ActiveStatus)
			log.Printf("  hash:    %s...", truncate(ms.SubscriberHash, 40))
		}
		if bundle.Benefits != nil {
			log.Printf("  benefits: annualMax=%v preventive=%v basic=%v major=%v",
				bundle.Benefits.AnnualMax,
				bundle.Benefits.PreventivePct,
				bundle.Benefits.BasicPct,
				bundle.Benefits.MajorPct,
			)
		}

		// Write probe file
		outPath := filepath.Join(outDir, fmt.Sprintf("pat%s_apt%s_bundle.json", appt.PatNum, appt.AptNum))
		writeJSON(outPath, bundle)
		log.Printf("  saved → %s", outPath)
	}

	hashCache.Flush()
	log.Printf("done — probe files in %s", outDir)
}

// ── query.8px.us helpers ──────────────────────────────────────────────────────

type queryRequest struct {
	Key   string `json:"key"`
	Query string `json:"query"`
}

type queryResponse struct {
	Data  []map[string]any `json:"data"`
	Error string           `json:"error,omitempty"`
}

func fetchAppointments(token, key string) ([]models.Appointment, error) {
	body, _ := json.Marshal(queryRequest{Key: key, Query: appointmentsQuery})
	req, _ := http.NewRequest("POST", "https://query.8px.us/api/run/query", bytes.NewReader(body))
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var qr queryResponse
	if err := json.Unmarshal(raw, &qr); err != nil {
		return nil, fmt.Errorf("decode: %w (body=%s)", err, truncate(string(raw), 200))
	}
	if qr.Error != "" {
		return nil, fmt.Errorf("query error: %s", qr.Error)
	}

	var result []models.Appointment
	for _, row := range qr.Data {
		a := models.Appointment{
			AptNum:       strVal(row, "aptNum"),
			PatNum:       strVal(row, "patNum"),
			FName:        strVal(row, "fName"),
			LName:        strVal(row, "lName"),
			DOB:          strVal(row, "dob"),
			SubscriberID: strVal(row, "subscriberId"),
			GroupNum:     strVal(row, "groupNum"),
			GroupName:    strVal(row, "groupName"),
			Relationship: strVal(row, "relationship"),
			TreatmentPlanProcCodes: strVal(row, "treatmentPlanProcCodes"),
		}
		result = append(result, a)
	}
	return result, nil
}

func strVal(row map[string]any, key string) string {
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int(t)) {
			return fmt.Sprintf("%d", int(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func writeJSON(path string, v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ensure unused import doesn't break build
var _ = strings.TrimSpace
