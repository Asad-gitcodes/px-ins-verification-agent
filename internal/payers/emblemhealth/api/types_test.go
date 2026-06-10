package api

import "testing"

func TestParseApexResultBulkRecords(t *testing.T) {
	raw := `[{"statusCode":200,"type":"rpc","tid":13,"ref":false,"action":"vlocity_ins.CardCanvasController","method":"doGenericInvoke","result":"{\"IPResult\":{\"response\":{\"memberEligibilitySerachRecords\":[{\"memberId\":\"K6082276202\",\"memberAltId\":\"K6082276202\",\"status\":\"Active\"},{\"memberId\":\"K6146807701\",\"memberAltId\":\"K6146807701\",\"status\":\"Active\"}]},\"size\":true,\"emptyResponse\":false},\"errorCode\":\"INVOKE-200\",\"error\":\"OK\"}"}]`

	result, err := ParseApexResult(raw)
	if err != nil {
		t.Fatalf("ParseApexResult: %v", err)
	}
	records := MatchRecordsByMemberID(result)
	if len(records) != 2 {
		t.Fatalf("records=%d, want 2", len(records))
	}
	if got := records["K6146807701"].Status; got != "Active" {
		t.Fatalf("status=%q, want Active", got)
	}
}

func TestParseApexResultSingleRecordObject(t *testing.T) {
	raw := `[{"statusCode":200,"type":"rpc","tid":12,"ref":false,"action":"vlocity_ins.CardCanvasController","method":"doGenericInvoke","result":"{\"IPResult\":{\"response\":{\"memberEligibilitySerachRecords\":{\"memberId\":\"K6150307201\",\"memberAltId\":\"K6150307201\",\"status\":\"Active\",\"memberFirstName\":\"Bueno\",\"memberLastName\":\"Allison\"}},\"size\":true,\"emptyResponse\":false},\"errorCode\":\"INVOKE-200\",\"error\":\"OK\"}"}]`

	result, err := ParseApexResult(raw)
	if err != nil {
		t.Fatalf("ParseApexResult: %v", err)
	}
	records := MatchRecordsByMemberID(result)
	if len(records) != 1 {
		t.Fatalf("records=%d, want 1", len(records))
	}
	got := records["K6150307201"]
	if got.Status != "Active" {
		t.Fatalf("status=%q, want Active", got.Status)
	}
	if got.MemberFirstName != "Bueno" {
		t.Fatalf("memberFirstName=%q, want Bueno", got.MemberFirstName)
	}
}

func TestParseApexResultNotFound(t *testing.T) {
	raw := `[{"statusCode":200,"type":"rpc","tid":14,"ref":false,"action":"vlocity_ins.CardCanvasController","method":"doGenericInvoke","result":"{\"IPResult\":{\"emptyResponse\":true,\"response\":{\"hits\":{\"total\":0,\"hits\":[]}},\"userId\":\"005Hp00000mhI0QIAU\"},\"errorCode\":\"INVOKE-200\",\"error\":\"OK\"}"}]`

	result, err := ParseApexResult(raw)
	if err != nil {
		t.Fatalf("ParseApexResult: %v", err)
	}
	if !result.IPResult.EmptyResponse {
		t.Fatalf("emptyResponse=false, want true")
	}
	if got := len(MatchRecordsByMemberID(result)); got != 0 {
		t.Fatalf("matched records=%d, want 0", got)
	}
}
