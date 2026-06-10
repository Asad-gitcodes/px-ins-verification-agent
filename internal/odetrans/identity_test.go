package odetrans

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"insurance-benefit-agent-go/internal/models"
)

func TestResolveIdentityUsesProvidedPayload(t *testing.T) {
	identity, err := ResolveIdentity(context.Background(), nil, "office", OfficeIdentity{
		Provider: ProviderIdentity{
			FirstName: "Rachna",
			LastName:  "Surana",
			TaxID:     "461277465",
			NPI:       "1912143538",
			Taxonomy:  "1223G0001X",
		},
		Practice: PracticeIdentity{
			Address: "30021 Alicia Parkway",
			City:    "Laguna Niguel",
			State:   "CA",
			Zip:     "92677",
		},
	})
	if err != nil {
		t.Fatalf("ResolveIdentity returned error for complete payload: %v", err)
	}
	if identity.Provider.LastName != "Surana" || identity.Practice.City != "Laguna Niguel" {
		t.Fatalf("identity=%+v", identity)
	}
}

func TestResolveIdentityQueriesMissingProviderAndPractice(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		queries = append(queries, body.Query)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "FROM provider"):
			_, _ = w.Write([]byte(`{"data":[{"LName":"Surana","FName":"Rachna","SSN":"461277465","NationalProvID":"1912143538","TaxonomyCodeOverride":""}]}`))
		case strings.Contains(body.Query, "FROM preference"):
			_, _ = w.Write([]byte(`{"data":[
				{"PrefName":"PracticeAddress","ValueString":"30021 Alicia Parkway"},
				{"PrefName":"PracticeCity","ValueString":"Laguna Niguel"},
				{"PrefName":"PracticeST","ValueString":"CA"},
				{"PrefName":"PracticeZip","ValueString":"92677"}
			]}`))
		default:
			t.Fatalf("unexpected query: %s", body.Query)
		}
	}))
	defer server.Close()

	identity, err := ResolveIdentity(context.Background(), &models.ScraperConfig{
		APIs: map[string]any{
			"query": map[string]any{
				"url":   server.URL,
				"token": "token",
			},
		},
	}, "office", OfficeIdentity{})
	if err != nil {
		t.Fatalf("ResolveIdentity error: %v", err)
	}
	if identity.Provider.FirstName != "Rachna" ||
		identity.Provider.LastName != "Surana" ||
		identity.Provider.TaxID != "461277465" ||
		identity.Provider.NPI != "1912143538" ||
		identity.Provider.Taxonomy != defaultDentalTaxonomy {
		t.Fatalf("provider=%+v", identity.Provider)
	}
	if identity.Practice.Address != "30021 Alicia Parkway" ||
		identity.Practice.City != "Laguna Niguel" ||
		identity.Practice.State != "CA" ||
		identity.Practice.Zip != "92677" {
		t.Fatalf("practice=%+v", identity.Practice)
	}
	if len(queries) != 2 {
		t.Fatalf("queries=%d, want 2: %v", len(queries), queries)
	}
}
