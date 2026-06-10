package odetrans

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"insurance-benefit-agent-go/internal/models"
)

func TestPersistPairRunsSavedQueriesInOrder(t *testing.T) {
	var calls []struct {
		Path string
		Body map[string]any
	}
	responses := map[string]any{
		"/api/query/execute/" + edi270QueryID:         map[string]any{"data": []map[string]any{{"msg270": 101}}},
		"/api/query/execute/" + etrans270QueryID:      map[string]any{"data": []map[string]any{{"etrans270": 202}}},
		"/api/query/execute/" + edi271QueryID:         map[string]any{"data": []map[string]any{{"msg271": 303}}},
		"/api/query/execute/" + etrans271QueryID:      map[string]any{"data": []map[string]any{{"etrans271": 404}}},
		"/api/query/execute/" + linkEtransPairQueryID: map[string]any{"ok": true},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token-123" {
			t.Fatalf("Authorization header=%q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		calls = append(calls, struct {
			Path string
			Body map[string]any
		}{Path: r.URL.Path, Body: body})
		resp, ok := responses[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	result, err := PersistPair(context.Background(), &models.ScraperConfig{
		APIs: map[string]any{
			"query": map[string]any{
				"url":   server.URL + "/api/run/query",
				"token": "token-123",
			},
		},
	}, "office-key", models.Appointment{
		CarrierNum: "230",
		PatNum:     "6812",
		PlanNum:    "3047",
		InsSubNum:  "2979",
	}, Pair{
		Request270:  "ST*270*0001~",
		Response271: "ST*271*0001~",
	}, time.Date(2026, 5, 13, 8, 9, 10, 0, time.UTC))
	if err != nil {
		t.Fatalf("PersistPair error: %v", err)
	}
	if result != (PersistResult{Msg270: 101, Etrans270: 202, Msg271: 303, Etrans271: 404}) {
		t.Fatalf("PersistResult=%+v", result)
	}

	gotPaths := make([]string, 0, len(calls))
	for _, call := range calls {
		gotPaths = append(gotPaths, call.Path)
	}
	wantPaths := []string{
		"/api/query/execute/" + edi270QueryID,
		"/api/query/execute/" + etrans270QueryID,
		"/api/query/execute/" + edi271QueryID,
		"/api/query/execute/" + etrans271QueryID,
		"/api/query/execute/" + linkEtransPairQueryID,
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("paths=%v", gotPaths)
	}
	if calls[0].Body["edi270"] != "ST*270*0001~" || calls[0].Body["licenseKey"] != "office-key" {
		t.Fatalf("unexpected edi270 payload: %#v", calls[0].Body)
	}
	if calls[1].Body["datetime_trans"] != "2026-05-13 08:09:10" || calls[1].Body["msg270"] != float64(101) {
		t.Fatalf("unexpected etrans270 payload: %#v", calls[1].Body)
	}
	if calls[1].Body["carrier_num"] != float64(230) || calls[1].Body["pat_num"] != float64(6812) {
		t.Fatalf("unexpected appointment ids: %#v", calls[1].Body)
	}
	if calls[1].Body["user_num"] != float64(1) {
		t.Fatalf("unexpected user_num: %#v", calls[1].Body)
	}
	if calls[4].Body["etrans270"] != float64(202) || calls[4].Body["etrans271"] != float64(404) {
		t.Fatalf("unexpected link payload: %#v", calls[4].Body)
	}
}

func TestSavedQueryBaseURLDerivesExecuteEndpoint(t *testing.T) {
	got := savedQueryBaseURL("https://super.8px.us/api/run/query")
	want := "https://super.8px.us/api/query/execute"
	if got != want {
		t.Fatalf("savedQueryBaseURL=%q want %q", got, want)
	}
}

func TestExtractIntHandlesCommonResponseShapes(t *testing.T) {
	for _, raw := range []string{
		`{"data":[{"msg270":42}]}`,
		`{"ResultData":[{"LAST_INSERT_ID()":"42"}]}`,
		`{"insertId":42}`,
		`{"DataFetchQuery":"SU5TRVJUIA==","ResultData":null,"ReturnId":42,"Resultdataset":null}`,
	} {
		got, ok := extractInt(json.RawMessage(raw), "msg270")
		if !ok || got != 42 {
			t.Fatalf("extractInt(%s)=%d,%v", raw, got, ok)
		}
	}
}
