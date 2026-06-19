package browser

import (
	"log"
	"strings"

	"insurance-benefit-agent-go/internal/logging"
	"insurance-benefit-agent-go/internal/payers/dentaquest/eligibility"
)

// scrapeTreatmentHistoryFromNetwork parses the stored clinical-history XHR
// payload into a map of procedureCode → []TreatmentHistoryEntry.
// Returns nil if no payload was captured.
func (s *Session) scrapeTreatmentHistoryFromNetwork(maxRows int) map[string][]eligibility.TreatmentHistoryEntry {
	stored := s.GetPayload("clinical-history")
	if stored == nil || stored.Payload == nil {
		log.Printf("[DentaQuest] clinical-history payload not captured")
		logging.Warn("dentaquest.browser", "dentaquest.member.treatment_history.missing", "clinical history payload not captured", nil)
		return nil
	}

	candidates := collectClinicalHistoryArrays(stored.Payload)
	if len(candidates) == 0 {
		log.Printf("[DentaQuest] clinical-history payload found but no record arrays matched")
		logging.Warn("dentaquest.browser", "dentaquest.member.treatment_history.no_arrays", "clinical history payload contained no matching record arrays", nil)
		return make(map[string][]eligibility.TreatmentHistoryEntry)
	}

	best := candidates[0]
	for _, c := range candidates[1:] {
		if len(c) > len(best) {
			best = c
		}
	}

	rows := mapClinicalHistoryRecords(best, maxRows)
	if len(rows) == 0 {
		log.Printf("[DentaQuest] clinical-history payload had no usable rows")
		logging.Warn("dentaquest.browser", "dentaquest.member.treatment_history.no_rows", "clinical history payload had no usable rows", nil)
		return make(map[string][]eligibility.TreatmentHistoryEntry)
	}

	result := buildHistoryByCode(rows)
	log.Printf("[DentaQuest] parsed %d treatment-history rows (%d unique codes)", len(rows), len(result))
	logging.Info("dentaquest.browser", "dentaquest.member.treatment_history.parsed", "parsed treatment history", map[string]any{
		"rows":        len(rows),
		"uniqueCodes": len(result),
	})
	return result
}

// collectClinicalHistoryArrays recursively searches v for arrays whose records
// contain both a service-date field and a procedure-code field.
func collectClinicalHistoryArrays(v any) [][]any {
	switch typed := v.(type) {
	case []any:
		if looksLikeClinicalHistory(typed) {
			return [][]any{typed}
		}
		var all [][]any
		for _, item := range typed {
			all = append(all, collectClinicalHistoryArrays(item)...)
		}
		return all
	case map[string]any:
		var all [][]any
		for _, val := range typed {
			all = append(all, collectClinicalHistoryArrays(val)...)
		}
		return all
	}
	return nil
}

func looksLikeClinicalHistory(arr []any) bool {
	for _, item := range arr {
		m := asStringMap(item)
		if m == nil {
			continue
		}
		date := anyStr(m, "dateOfService", "serviceDate", "dos", "date")
		code := anyStr(m, "procedureCode", "procCode", "code", "cdtCode")
		if date != "" && code != "" {
			return true
		}
	}
	return false
}

type rawHistoryRow struct {
	ServiceDate   string
	ProcedureCode string
	Description   string
	PartOfMouth   string
}

func mapClinicalHistoryRecords(records []any, max int) []rawHistoryRow {
	var rows []rawHistoryRow
	for _, r := range records {
		m := asStringMap(r)
		if m == nil {
			continue
		}
		row := rawHistoryRow{
			ServiceDate:   normalizeSpace(anyStr(m, "dateOfService", "serviceDate", "dos", "date")),
			ProcedureCode: normalizeSpace(anyStr(m, "procedureCode", "procCode", "code", "cdtCode")),
			Description:   normalizeSpace(anyStr(m, "procedureDescription", "description", "desc")),
			PartOfMouth:   normalizeSpace(anyStr(m, "partOfMouth", "toothArchQuadSurface", "toothSurface", "tooth")),
		}
		if row.ServiceDate == "" || row.ProcedureCode == "" {
			continue
		}
		rows = append(rows, row)
		if len(rows) >= max {
			break
		}
	}
	return rows
}

func buildHistoryByCode(rows []rawHistoryRow) map[string][]eligibility.TreatmentHistoryEntry {
	result := make(map[string][]eligibility.TreatmentHistoryEntry)
	for _, row := range rows {
		parsed := parsePartOfMouth(row.PartOfMouth)
		result[row.ProcedureCode] = append(result[row.ProcedureCode], eligibility.TreatmentHistoryEntry{
			ServiceDate:      row.ServiceDate,
			ToothCode:        parsed.toothCode,
			ToothDescription: parsed.toothDescription,
			Surfaces:         parsed.surfaces,
		})
	}
	return result
}

type partOfMouthParsed struct {
	toothCode        string
	toothDescription string
	surfaces         string
}

func parsePartOfMouth(value string) partOfMouthParsed {
	v := normalizeSpace(value)
	if v == "" || v == "-- / -- / -- / --" {
		return partOfMouthParsed{}
	}
	parts := strings.Split(v, "/")
	get := func(i int) string {
		if i < len(parts) {
			if s := strings.TrimSpace(parts[i]); s != "--" {
				return s
			}
		}
		return ""
	}
	var descParts []string
	if a := get(1); a != "" {
		descParts = append(descParts, a)
	}
	if q := get(2); q != "" {
		descParts = append(descParts, q)
	}
	return partOfMouthParsed{
		toothCode:        get(0),
		toothDescription: strings.Join(descParts, " / "),
		surfaces:         get(3),
	}
}
