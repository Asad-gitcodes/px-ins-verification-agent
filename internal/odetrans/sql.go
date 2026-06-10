package odetrans

import (
	"fmt"
	"strconv"
	"strings"

	"insurance-benefit-agent-go/internal/models"
)

const syntheticClearingHouseNum = 20

// BuildInsertSQL renders a manual-review SQL script for inserting the generated
// 270/271 pair into Open Dental. It does not execute anything.
func BuildInsertSQL(appt models.Appointment, pair Pair) string {
	carrierNum := sqlInt(appt.CarrierNum)
	patNum := sqlInt(appt.PatNum)
	planNum := sqlInt(appt.PlanNum)
	insSubNum := sqlInt(appt.InsSubNum)

	var b strings.Builder
	b.WriteString("-- Synthetic eligibility eTrans insert script generated for manual review.\n")
	b.WriteString("-- Verify values before running against Open Dental.\n")
	b.WriteString("START TRANSACTION;\n\n")
	b.WriteString("INSERT INTO etransmessagetext (MessageText)\n")
	b.WriteString("VALUES (")
	b.WriteString(sqlQuote(pair.Request270))
	b.WriteString(");\n")
	b.WriteString("SET @msg270 := LAST_INSERT_ID();\n\n")
	b.WriteString("INSERT INTO etrans (\n")
	b.WriteString("  DateTimeTrans, ClearingHouseNum, Etype, ClaimNum, OfficeSequenceNumber,\n")
	b.WriteString("  CarrierTransCounter, CarrierTransCounter2, CarrierNum, CarrierNum2, PatNum,\n")
	b.WriteString("  BatchNumber, AckCode, TransSetNum, Note, EtransMessageTextNum, AckEtransNum,\n")
	b.WriteString("  PlanNum, InsSubNum, TranSetId835, CarrierNameRaw, PatientNameRaw, UserNum\n")
	b.WriteString(")\n")
	b.WriteString(fmt.Sprintf("VALUES (\n  NOW(), %d, 24, 0, 0,\n  0, 0, %d, 0, %d,\n  0, '', 0, '', @msg270, 0,\n  %d, %d, '', '', '', 0\n);\n",
		syntheticClearingHouseNum, carrierNum, patNum, planNum, insSubNum))
	b.WriteString("SET @etrans270 := LAST_INSERT_ID();\n\n")
	b.WriteString("INSERT INTO etransmessagetext (MessageText)\n")
	b.WriteString("VALUES (")
	b.WriteString(sqlQuote(pair.Response271))
	b.WriteString(");\n")
	b.WriteString("SET @msg271 := LAST_INSERT_ID();\n\n")
	b.WriteString("INSERT INTO etrans (\n")
	b.WriteString("  DateTimeTrans, ClearingHouseNum, Etype, ClaimNum, OfficeSequenceNumber,\n")
	b.WriteString("  CarrierTransCounter, CarrierTransCounter2, CarrierNum, CarrierNum2, PatNum,\n")
	b.WriteString("  BatchNumber, AckCode, TransSetNum, Note, EtransMessageTextNum, AckEtransNum,\n")
	b.WriteString("  PlanNum, InsSubNum, TranSetId835, CarrierNameRaw, PatientNameRaw, UserNum\n")
	b.WriteString(")\n")
	b.WriteString(fmt.Sprintf("VALUES (\n  NOW(), %d, 25, 0, 0,\n  0, 0, %d, 0, %d,\n  0, '', 0, '', @msg271, 0,\n  %d, %d, '', '', '', 0\n);\n",
		syntheticClearingHouseNum, carrierNum, patNum, planNum, insSubNum))
	b.WriteString("SET @etrans271 := LAST_INSERT_ID();\n\n")
	b.WriteString("UPDATE etrans\n")
	b.WriteString("SET AckEtransNum = @etrans271,\n")
	b.WriteString("    Note = 'Normal 271 response.'\n")
	b.WriteString("WHERE EtransNum = @etrans270;\n\n")
	b.WriteString("COMMIT;\n")
	return b.String()
}

func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func sqlInt(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
