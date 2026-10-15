package tdcloud

import (
	"encoding/json"
	"testing"
)

func TestLegacyInvoiceUnitsAreNotProjectedToMerchantEditor(t *testing.T) {
	raw := json.RawMessage(`{"api_url":"https://cloud.example.test","client_id":"merchant","client_secret":"secret","currencies":{"TDC":8}}`)
	view, err := ProjectSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]json.RawMessage
	_ = json.Unmarshal(view, &settings)
	if _, exists := settings["currencies"]; exists {
		t.Fatal("merchant editor must not expose invoice precision")
	}
	normalized, err := NormalizeSettings(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(normalized, &settings)
	if _, exists := settings["currencies"]; exists {
		t.Fatal("new config must not persist a manual override of TDCloud units")
	}
}
