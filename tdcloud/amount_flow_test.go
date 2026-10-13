package tdcloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func TestAmbiguousCreateRestoresPinnedPrecisionAndRejectsResponseMismatch(t *testing.T) {
	var directoryCalls, creates atomic.Int32
	var directoryDecimals, responseDecimals atomic.Int32
	directoryDecimals.Store(8)
	responseDecimals.Store(8)
	var responseAmount atomic.Int64
	responseAmount.Store(1050000000)
	var missingPrecision atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 300})
		case "/openapi/v1/merchant-payment-channels":
			directoryCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []MerchantChannel{{PaymentNo: "test", Currency: "TDC", CurrencyDecimals: intPointer(int(directoryDecimals.Load()))}}})
		case "/openapi/v1/merchant-payments":
			var request map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if string(request["amount_minor"]) != "1050000000" || string(request["idempotency_key"]) != `"same-key"` {
				t.Errorf("retry changed provider parameters: %s", request)
			}
			if creates.Add(1) == 1 {
				// Provider may already have created the order; no usable response.
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			view := merchantPayment{OrderNo: "mp", ExternalRef: "zb", PaymentNo: "test", Currency: "TDC", AmountMinor: responseAmount.Load(), CurrencyDecimals: intPointer(int(responseDecimals.Load())), Status: "pending", CheckoutURL: "https://checkout.example.test/1"}
			if missingPrecision.Load() {
				view.CurrencyDecimals = nil
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(view)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	raw, _ := json.Marshal(Settings{Currencies: map[string]int{"USD": 2, "CNY": 2, "TDC": 8, "JPY": 0}, APIURL: server.URL, ClientID: "client", ClientSecret: "secret", PaymentNo: "test"})
	active, err := NewFromSettings("tdcloud", raw, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	prepared, snapshot, err := active.PrepareSnapshot(t.Context(), raw, "TDC")
	if err != nil {
		t.Fatal(err)
	}
	request := payment.CheckoutRequest{Reference: "zb", IdempotencyKey: "same-key", AmountMinor: 1050, Currency: "TDC"}
	if _, err := prepared.CreateCheckout(t.Context(), request); err == nil {
		t.Fatal("ambiguous failure accepted")
	}
	directoryDecimals.Store(2)
	restored, err := NewFromSnapshot("tdcloud", snapshot, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.CreateCheckout(t.Context(), request); err != nil || directoryCalls.Load() != 0 {
		t.Fatalf("retry must use saved units without discovery: %v directory=%d", err, directoryCalls.Load())
	}
	for _, bad := range []string{"amount", "precision", "missing precision"} {
		switch bad {
		case "amount":
			responseAmount.Add(1)
		case "precision":
			responseDecimals.Store(2)
		case "missing precision":
			missingPrecision.Store(true)
		}
		if _, err := restored.CreateCheckout(t.Context(), request); err == nil {
			t.Fatalf("accepted checkout response with wrong %s", bad)
		}
		responseAmount.Store(1050000000)
		responseDecimals.Store(8)
	}
	if _, err := NormalizeSettings(snapshot, nil); err == nil {
		t.Fatal("user config accepted plugin-managed amount metadata")
	}
}

func TestLegacyInvoiceMetadataRejectsInvalidPrecisionIncludingZero(t *testing.T) {
	for _, units := range []string{`{"JPY":-1}`, `{"JPY":9}`, `{"JPY":0.5}`, `{"jpy":0}`} {
		raw := []byte(`{"api_url":"https://cloud.example.test","client_id":"client","client_secret":"secret","currencies":` + units + `}`)
		if _, err := NormalizeSettings(raw, nil); err == nil {
			t.Fatalf("invalid precision accepted: %s", units)
		}
	}
	if _, err := NormalizeSettings([]byte(`{"api_url":"https://cloud.example.test","client_id":"client","client_secret":"secret","currencies":{"JPY":0}}`), nil); err != nil {
		t.Fatalf("explicit zero precision rejected: %v", err)
	}
}
