package api

import (
	"encoding/json"
	"testing"
)

func TestProcedureCategoriesAcceptsObjectAdditionalDetails(t *testing.T) {
	raw := []byte(`{
		"insureds": [],
		"metaData": {
			"serviceReferenceId": "test-ref",
			"outcome": {
				"statusCode": 200,
				"message": "SUCCESS",
				"additionalDetails": {}
			}
		}
	}`)

	var parsed ProcedureCategoriesResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal procedure categories response: %v", err)
	}
}
