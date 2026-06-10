package vynetrellis

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

//go:embed carriers.json
var carriersJSON []byte

type carrierEntry struct {
	CarrierName string `json:"CarrierName"`
	CarrierId   string `json:"CarrierId"`
}

var (
	carrierIndex     map[string][]carrierEntry // key = lower(CarrierId)
	carrierIndexOnce sync.Once
)

func buildCarrierIndex() map[string][]carrierEntry {
	var payload struct {
		Carriers []carrierEntry `json:"Carriers"`
	}
	if err := json.Unmarshal(carriersJSON, &payload); err != nil {
		return nil
	}
	idx := make(map[string][]carrierEntry, len(payload.Carriers))
	for _, c := range payload.Carriers {
		key := strings.ToLower(strings.TrimSpace(c.CarrierId))
		idx[key] = append(idx[key], c)
	}
	return idx
}

// ResolveCarrier returns the canonical (carrierId, carrierName) pair for a given
// HIPAA payer ID. When multiple carriers share the same ID, nameHint (the carrier
// name Vyne or OpenDental already knows) is used to pick the closest match.
// Falls back to the first entry, then to (payerID, nameHint) if nothing found.
func ResolveCarrier(payerID, nameHint string) (string, string) {
	carrierIndexOnce.Do(func() {
		carrierIndex = buildCarrierIndex()
	})

	key := strings.ToLower(strings.TrimSpace(payerID))
	entries := carrierIndex[key]

	if len(entries) == 0 {
		return payerID, nameHint
	}
	if len(entries) == 1 {
		return entries[0].CarrierId, entries[0].CarrierName
	}

	// Multiple entries share this ID — find the closest name match.
	hint := strings.ToLower(strings.TrimSpace(nameHint))

	// 1. Exact match.
	for _, e := range entries {
		if strings.EqualFold(e.CarrierName, nameHint) {
			return e.CarrierId, e.CarrierName
		}
	}

	// 2. One contains the other.
	for _, e := range entries {
		lower := strings.ToLower(e.CarrierName)
		if strings.Contains(lower, hint) || strings.Contains(hint, lower) {
			return e.CarrierId, e.CarrierName
		}
	}

	// 3. Most words in common.
	hintWords := strings.Fields(hint)
	best, bestScore := entries[0], 0
	for _, e := range entries {
		score := 0
		lower := strings.ToLower(e.CarrierName)
		for _, w := range hintWords {
			if len(w) > 2 && strings.Contains(lower, w) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = e, score
		}
	}
	return best.CarrierId, best.CarrierName
}
