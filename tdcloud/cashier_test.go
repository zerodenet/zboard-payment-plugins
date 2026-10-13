package tdcloud

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func TestHostedCashierNeverChoosesInternalMethodAndPinsInvoiceUnits(t *testing.T) {
	var paid atomic.Bool
	var legacy atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 300})
		case "/openapi/v1/merchant-payment-channels":
			// No channel ID or invoice precision is exposed. This method accepts
			// USD while the outside business order is denominated in TDC.
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"name": "Card", "currency": "USD", "login_required": true}}})
		case "/openapi/v1/merchant-payments":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			method, present := body["payment_no"]
			if !legacy.Load() && present {
				t.Errorf("new order selected cashier method: %s", method)
			}
			if legacy.Load() && string(method) != `"original-method"` {
				t.Errorf("legacy retry changed original method: %s", method)
			}
			if string(body["amount_minor"]) != "1050000000" || string(body["currency"]) != `"TDC"` {
				t.Errorf("invoice units changed: %s", body)
			}
			methodID := ""
			if legacy.Load() {
				methodID = "original-method"
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp", ExternalRef: "zb", PaymentNo: methodID, AmountMinor: 1050000000, Currency: "TDC", CurrencyDecimals: intPointer(8), Status: "pending", CheckoutURL: "https://cashier.example.test/mp", ExpiresAt: time.Now().Add(time.Hour)})
		case "/openapi/v1/merchant-payments/mp":
			status, method := "pending", ""
			if paid.Load() {
				status, method = "paid", "selected-by-tdcloud"
			}
			_ = json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp", ExternalRef: "zb", PaymentNo: method, AmountMinor: 1050000000, Currency: "TDC", CurrencyDecimals: intPointer(8), Status: status, EventID: "event", ProviderTradeNo: "trade", OccurredAt: time.Now()})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	secret := "independent-notification-secret-123456789"
	raw, _ := json.Marshal(Settings{APIURL: server.URL, ClientID: "merchant", ClientSecret: "secret", PaymentNo: "must-not-use", Currencies: map[string]int{"TDC": 8}, NotifyURL: "https://board.example.test" + NotificationPath, NotifySecret: secret})
	normalized, err := NormalizeSettings(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	var activeSettings Settings
	_ = json.Unmarshal(normalized, &activeSettings)
	if activeSettings.PaymentNo != "" || len(activeSettings.Currencies) != 0 {
		t.Fatal("new configuration retained internal method or manual invoice units")
	}
	client, err := NewFromSettings("tdcloud", normalized, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	// This unit fixture supplies known provider metadata to exercise conversion
	// and cashier behavior; it does not claim automatic API discovery exists.
	client.currencies = map[string]int{"TDC": 8}
	info, err := client.Describe(t.Context())
	if err != nil || info.Currency != "TDC" || info.LoginRequired {
		t.Fatalf("cashier method was confused with invoice: %+v %v", info, err)
	}
	prepared, snapshot, err := client.PrepareSnapshot(t.Context(), normalized, "TDC")
	if err != nil {
		t.Fatal(err)
	}
	request := payment.CheckoutRequest{Reference: "zb", IdempotencyKey: "key", AmountMinor: 1050, Currency: "TDC"}
	if view, err := prepared.CreateCheckout(t.Context(), request); err != nil || view.ProviderOrderID != "mp" {
		t.Fatalf("unselected cashier creation: %+v %v", view, err)
	}
	if result, err := prepared.Lookup(t.Context(), "mp"); err != nil || result.Status != "pending" || result.AmountMinor != 1050 {
		t.Fatalf("unselected lookup: %+v %v", result, err)
	}
	paid.Store(true)
	if result, err := prepared.Lookup(t.Context(), "mp"); err != nil || result.Status != "paid" || result.AmountMinor != 1050 {
		t.Fatalf("cashier selection rejected: %+v %v", result, err)
	}
	// The signed notification's internal method ID cannot replace the order
	// identity or invoice units; it is not compared to a configured method.
	now := time.Now().UTC()
	body, _ := json.Marshal(map[string]any{"id": "notification", "type": "merchant_payment.status_changed", "api_version": 1, "created_at": now, "data": map[string]any{"order_no": "mp", "external_reference": "zb", "payment_no": "cashier-selected-method", "status": "paid", "status_version": 2, "amount_minor": 1050000000, "currency": "TDC", "currency_decimals": 8}})
	timestamp := strconv.FormatInt(now.Unix(), 10)
	headers := http.Header{"X-Tdcloud-Event-Id": {"notification"}, "X-Tdcloud-Timestamp": {timestamp}, "X-Tdcloud-Signature": {signedNotification(body, timestamp, secret)}}
	hint, err := prepared.(payment.NotificationChannel).InspectNotification(body, headers, now)
	if err != nil || hint.AmountMinor != 1050 {
		t.Fatalf("selected-method notification: %+v %v", hint, err)
	}
	var saved Settings
	_ = json.Unmarshal(snapshot, &saved)
	if saved.AmountMetadata == nil || saved.AmountMetadata.Currency != "TDC" || saved.AmountMetadata.Decimals != 8 || saved.PaymentNo != "" {
		t.Fatal("invoice metadata not pinned independently of cashier")
	}
	// Preserve an already-issued legacy attempt's exact request parameters.
	saved.PaymentNo = "original-method"
	legacyRaw, _ := json.Marshal(saved)
	restored, err := NewFromSnapshot("tdcloud", legacyRaw, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	legacy.Store(true)
	if _, err := restored.CreateCheckout(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Lookup(t.Context(), "mp"); err == nil {
		t.Fatal("legacy snapshot lost original method binding")
	}
	if _, err := client.resolveAmount(t.Context(), "USD"); !errors.Is(err, payment.ErrConflict) {
		t.Fatal("unsupported invoice currency accepted")
	}
}
