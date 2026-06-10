package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"insurance-benefit-agent-go/internal/config"
)

func TestTrackingUsesMainTokenFromServerConfig(t *testing.T) {
	const (
		officeKey      = "office-123"
		bootstrapToken = "Bearer bootstrap-token"
		mainToken      = "Bearer main-token"
		payerID        = "payer-456"
		trackingID     = "tracking-789"
	)

	var startAuth string
	var endAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/config/insSched/details/" + officeKey:
			if got := r.Header.Get("Authorization"); got != bootstrapToken {
				t.Fatalf("config Authorization=%q, want %q", got, bootstrapToken)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"UserId":         17,
				"MAIN_DOMAIN":    "http://" + r.Host,
				"MAIN_API_TOKEN": mainToken,
			})
		case "/api/config/start/qispayor/tracking/17/" + payerID:
			startAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(trackingID)
		case "/api/config/end/qispayor/tracking/17/" + payerID + "/" + trackingID:
			endAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(&config.Config{
		OfficeKey: officeKey,
		Bootstrap: config.BootstrapConfig{Patcon: config.PatconConfig{
			URL:   server.URL,
			Token: bootstrapToken,
		}},
	})

	if _, err := client.FetchServerConfig(context.Background()); err != nil {
		t.Fatalf("FetchServerConfig: %v", err)
	}
	id, err := client.StartPayerTracking(context.Background(), 17, payerID)
	if err != nil {
		t.Fatalf("StartPayerTracking: %v", err)
	}
	if id != trackingID {
		t.Fatalf("tracking id=%q, want %q", id, trackingID)
	}
	if err := client.EndPayerTracking(context.Background(), 17, trackingID, payerID, "Complete", map[string]any{}); err != nil {
		t.Fatalf("EndPayerTracking: %v", err)
	}

	if startAuth != mainToken {
		t.Fatalf("start Authorization=%q, want %q", startAuth, mainToken)
	}
	if endAuth != mainToken {
		t.Fatalf("end Authorization=%q, want %q", endAuth, mainToken)
	}
}
