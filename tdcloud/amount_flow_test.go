package tdcloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func TestDecimalRetryRestoresWireContractWithoutProviderPrecision(t *testing.T) {
	var creates, directoryCalls atomic.Int32
	var response atomic.Value
	response.Store("10.50000000")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 300})
		case "/openapi/v1/merchant-payment-channels":
			directoryCalls.Add(1)
			http.NotFound(w, r)
		case "/openapi/v1/merchant-payments":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if string(body["amount"]) != `"10.50"` || body["amount_minor"] != nil || body["payment_no"] != nil || string(body["idempotency_key"]) != `"same-key"` {
				t.Errorf("unexpected retry parameters: %s", body)
			}
			if creates.Add(1) == 1 {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp", ExternalRef: "zb", Currency: "TDC", Amount: response.Load().(string), Status: "pending", CheckoutURL: "https://cashier.example.test/mp"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	raw, _ := json.Marshal(Settings{APIURL: server.URL, ClientID: "client", ClientSecret: "secret"})
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
	restored, err := NewFromSnapshot("tdcloud", snapshot, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.CreateCheckout(t.Context(), request); err != nil || directoryCalls.Load() != 0 {
		t.Fatalf("retry queried provider precision: %v", err)
	}
	for _, bad := range []string{"10.51", "10.50000001", "1.05", "", "1e1", "-10.50", "10.50 "} {
		response.Store(bad)
		if _, err := restored.CreateCheckout(t.Context(), request); err == nil {
			t.Fatalf("accepted response amount %q", bad)
		}
	}
	if _, err := NormalizeSettings(snapshot, nil); err == nil {
		t.Fatal("merchant injected private wire metadata")
	}
}

func TestHistoricalIntegerRetriesKeepOriginalUnitsAndMethod(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(map[bool]string{false: "unversioned", true: "v1"}[versioned], func(t *testing.T) {
			expected, decimals := int64(1050), 2
			if versioned {
				expected, decimals = 1050000000, 8
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 300})
					return
				}
				if r.URL.Path != "/openapi/v1/merchant-payments" {
					t.Error("legacy retry performed discovery")
					http.NotFound(w, r)
					return
				}
				var body map[string]json.RawMessage
				json.NewDecoder(r.Body).Decode(&body)
				if string(body["amount_minor"]) != map[bool]string{false: "1050", true: "1050000000"}[versioned] || body["amount"] != nil || string(body["payment_no"]) != `"original-method"` || body["notify_enabled"] != nil {
					t.Errorf("legacy wire changed: %s", body)
				}
				w.WriteHeader(201)
				json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp", ExternalRef: "zb", PaymentNo: "original-method", Currency: "TDC", AmountMinor: expected, CurrencyDecimals: intPointer(decimals), Status: "pending"})
			}))
			defer server.Close()
			settings := Settings{APIURL: server.URL, ClientID: "client", ClientSecret: "secret", PaymentNo: "original-method"}
			if versioned {
				settings.AmountMetadata = &AmountMetadata{Version: 1, Currency: "TDC", Decimals: 8}
			}
			raw, _ := json.Marshal(settings)
			restored, err := NewFromSnapshot("tdcloud", raw, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restored.CreateCheckout(t.Context(), payment.CheckoutRequest{Reference: "zb", IdempotencyKey: "legacy-key", AmountMinor: 1050, Currency: "TDC"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
