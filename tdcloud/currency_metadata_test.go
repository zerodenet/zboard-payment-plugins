package tdcloud

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func TestMerchantConfigurationDoesNotRequireInvoiceUnitsOrGuessThem(t *testing.T) {
	var creates atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi/v1/merchant-payments" {
			creates.Add(1)
		}
		// A cashier-method directory cannot define invoice precision.
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []MerchantChannel{{Currency: "TDC", CurrencyDecimals: intPointer(8)}}})
	}))
	defer provider.Close()
	raw, _ := json.Marshal(Settings{APIURL: provider.URL, ClientID: "merchant", ClientSecret: "secret"})
	normalized, err := NormalizeSettings(raw, nil)
	if err != nil {
		t.Fatalf("merchant should only configure connection and credentials: %v", err)
	}
	client, err := NewFromSettings("tdcloud", normalized, provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Describe(t.Context()); !errors.Is(err, ErrCurrencyMetadataUnavailable) {
		t.Fatalf("missing invoice source must not publish a payment option: %v", err)
	}
	if _, _, err := client.PrepareSnapshot(t.Context(), normalized, "TDC"); !errors.Is(err, ErrCurrencyMetadataUnavailable) {
		t.Fatalf("missing invoice source must not prepare a guessed amount: %v", err)
	}
	if _, err := client.CreateCheckout(t.Context(), payment.CheckoutRequest{Reference: "zb", IdempotencyKey: "key", AmountMinor: 1050, Currency: "TDC"}); !errors.Is(err, ErrCurrencyMetadataUnavailable) || creates.Load() != 0 {
		t.Fatalf("must reject before creating an incorrectly denominated order: %v creates=%d", err, creates.Load())
	}
}

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
