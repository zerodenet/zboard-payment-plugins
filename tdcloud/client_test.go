package tdcloud

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func TestMerchantPaymentCreateAndLookup(t *testing.T) {
	var creates int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if r.FormValue("grant_type") != "client_credentials" || r.FormValue("scope") != "merchant_payments.create merchant_payments.read" {
				t.Error("incorrect OAuth request")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 3600})
		case "/openapi/v1/merchant-payments":
			creates++
			if r.Header.Get("Authorization") != "Bearer token" {
				t.Error("missing authenticated merchant token")
			}
			var request struct {
				Reference     string `json:"external_reference"`
				Description   string `json:"description"`
				ReturnURL     string `json:"return_url"`
				NotifyEnabled *bool  `json:"notify_enabled"`
				Key           string `json:"idempotency_key"`
				Amount        int64  `json:"amount_minor"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Reference != "zboard-7" || request.Description != "Monthly plan" || request.ReturnURL != "https://panel.example.test/orders/7" || request.Key != "fixed-key" || request.Amount != 1200 || request.NotifyEnabled == nil || *request.NotifyEnabled {
				t.Errorf("invalid create request: %+v, %v", request, err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp123", ExternalRef: request.Reference, ReturnURL: request.ReturnURL, PaymentNo: "stripe-1", AmountMinor: 1200,
				Currency: "CNY", CurrencyDecimals: intPointer(2), Status: "pending", CheckoutURL: "https://checkout.example.test/1", ExpiresAt: time.Now().Add(time.Hour)})
		case "/openapi/v1/merchant-payments/mp123":
			_ = json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp123", ExternalRef: "zboard-7", PaymentNo: "stripe-1", AmountMinor: 1200,
				Currency: "CNY", CurrencyDecimals: intPointer(2), Status: "paid", EventID: "event-1", ProviderTradeNo: "cs-1", OccurredAt: time.Now().UTC()})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []MerchantChannel{{PaymentNo: "stripe-1", Currency: "CNY", CurrencyDecimals: intPointer(2)}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New("tdcloud", server.URL, "client", "secret", map[string]int{"CNY": 2, "USD": 2}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkout, err := client.CreateCheckout(t.Context(), payment.CheckoutRequest{Reference: "zboard-7", Description: "Monthly plan", ReturnURL: "https://panel.example.test/orders/7", IdempotencyKey: "fixed-key", AmountMinor: 1200, Currency: "CNY"})
	if err != nil || checkout.ProviderOrderID != "mp123" || checkout.RedirectURL != "https://checkout.example.test/1" || checkout.Status != "pending" || creates != 1 {
		t.Fatalf("checkout: %+v, %v, creates=%d", checkout, err, creates)
	}
	result, err := client.Lookup(t.Context(), checkout.ProviderOrderID)
	if err != nil || result.Status != "paid" || result.AmountMinor != 1200 || result.ProviderTradeNo != "cs-1" {
		t.Fatalf("verified result: %+v, %v", result, err)
	}
}

func TestExplicitNotificationSettingsAreSentWithCheckout(t *testing.T) {
	secret := "independent-merchant-notification-secret-123456"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 3600})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []MerchantChannel{{PaymentNo: "stripe-1", Currency: "CNY", CurrencyDecimals: intPointer(2)}}})
		case "/openapi/v1/merchant-payments":
			var request struct {
				NotifyEnabled bool   `json:"notify_enabled"`
				NotifyURL     string `json:"notify_url"`
				NotifySecret  string `json:"notify_secret"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if !request.NotifyEnabled || request.NotifyURL != "https://hooks.example.test"+NotificationPath || request.NotifySecret != secret {
				t.Errorf("notification settings not sent: %+v", request)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp123", ExternalRef: "zb-7", PaymentNo: "stripe-1", AmountMinor: 1200,
				Currency: "CNY", CurrencyDecimals: intPointer(2), Status: "pending", CheckoutURL: "https://checkout.example.test/1", ExpiresAt: time.Now().Add(time.Hour)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	raw, _ := json.Marshal(Settings{Currencies: map[string]int{"USD": 2, "CNY": 2, "TDC": 8, "JPY": 0}, APIURL: server.URL, ClientID: "client", ClientSecret: "oauth-secret", PaymentNo: "stripe-1",
		NotifyURL: "https://hooks.example.test" + NotificationPath, NotifySecret: secret})
	client, err := NewFromSettings("tdcloud", raw, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateCheckout(t.Context(), payment.CheckoutRequest{Reference: "zb-7", IdempotencyKey: "key-7", AmountMinor: 1200, Currency: "CNY"}); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationSettingsRequireIndependentSecretAndStableURL(t *testing.T) {
	base := Settings{Currencies: map[string]int{"USD": 2, "CNY": 2, "TDC": 8, "JPY": 0}, APIURL: "https://cloud.example.test", ClientID: "client", ClientSecret: "oauth-secret", PaymentNo: "stripe-1",
		NotifyURL: "https://hooks.example.test" + NotificationPath, NotifySecret: "independent-merchant-notification-secret-123456"}
	raw, _ := json.Marshal(base)
	if _, err := NormalizeSettings(raw, nil); err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizeSettings(raw, nil)
	var active Settings
	if err != nil || json.Unmarshal(normalized, &active) != nil || active.NotifyEnabled == nil || !*active.NotifyEnabled {
		t.Fatalf("configured endpoint must pin enabled notification choice: %+v %v", active, err)
	}
	base.NotifySecret = ""
	updated, _ := json.Marshal(base)
	normalized, err = NormalizeSettings(updated, raw)
	var retained Settings
	if err != nil || json.Unmarshal(normalized, &retained) != nil || retained.NotifySecret != "independent-merchant-notification-secret-123456" {
		t.Fatalf("same endpoint must preserve secret: %+v %v", retained, err)
	}
	base.NotifyURL = "https://other.example.test" + NotificationPath
	updated, _ = json.Marshal(base)
	if _, err := NormalizeSettings(updated, raw); err == nil {
		t.Fatal("legacy per-order key must not transfer to a different endpoint")
	}
	base.NotifyURL = "https://hooks.example.test" + NotificationPath
	base.NotifySecret = base.ClientSecret
	updated, _ = json.Marshal(base)
	if _, err := NormalizeSettings(updated, nil); err == nil {
		t.Fatal("OAuth secret must not be reused for notifications")
	}
	base.NotifySecret = "independent-merchant-notification-secret-123456"
	disabled := false
	base.NotifyEnabled = &disabled
	updated, _ = json.Marshal(base)
	if _, err := NormalizeSettings(updated, nil); err == nil {
		t.Fatal("notification endpoint cannot be disabled for the same order")
	}
	base.NotifyEnabled = nil
	base.NotifyURL = "https://localhost" + NotificationPath
	updated, _ = json.Marshal(base)
	if _, err := NormalizeSettings(updated, nil); err == nil {
		t.Fatal("private notification endpoint must be rejected")
	}
}

func TestLegacyCheckoutSnapshotKeepsOmittedNotificationChoice(t *testing.T) {
	createBodies := make(chan map[string]any, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 3600})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []MerchantChannel{{PaymentNo: "stripe-1", Currency: "CNY", CurrencyDecimals: intPointer(2)}}})
		case "/openapi/v1/merchant-payments":
			var createBody map[string]any
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			createBodies <- createBody
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp123", ExternalRef: "zb-7", PaymentNo: "stripe-1", AmountMinor: 1200,
				Currency: "CNY", CurrencyDecimals: intPointer(2), Status: "pending", CheckoutURL: "https://checkout.example.test/1", ExpiresAt: time.Now().Add(time.Hour)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	legacy, _ := json.Marshal(Settings{Currencies: map[string]int{"USD": 2, "CNY": 2, "TDC": 8, "JPY": 0}, APIURL: server.URL, ClientID: "client", ClientSecret: "oauth-secret", PaymentNo: "stripe-1"})
	channel, err := NewFromSettings("tdcloud", legacy, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	request := payment.CheckoutRequest{Reference: "zb-7", IdempotencyKey: "key-7", AmountMinor: 1200, Currency: "CNY"}
	if _, err := channel.CreateCheckout(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	createBody := <-createBodies
	if _, present := createBody["notify_enabled"]; present {
		t.Fatal("legacy attempt retry changed TDCloud idempotency parameters")
	}
	current, err := NormalizeSettings(legacy, nil)
	var settings Settings
	if err != nil || json.Unmarshal(current, &settings) != nil || settings.NotifyEnabled == nil || *settings.NotifyEnabled {
		t.Fatalf("new config must opt out of merchant notification defaults: %+v %v", settings, err)
	}
}

func TestCheckoutRejectsUnexpectedReturnURL(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 3600})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []MerchantChannel{{PaymentNo: "stripe-1", Currency: "CNY", CurrencyDecimals: intPointer(2)}}})
		case "/openapi/v1/merchant-payments":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp123", ExternalRef: "zboard-7", ReturnURL: "https://wrong.example.test/orders/7",
				PaymentNo: "stripe-1", AmountMinor: 1200, Currency: "CNY", CurrencyDecimals: intPointer(2), Status: "pending", CheckoutURL: "https://checkout.example.test/1", ExpiresAt: time.Now().Add(time.Hour)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New("tdcloud", server.URL, "client", "secret", map[string]int{"CNY": 2, "USD": 2}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateCheckout(t.Context(), payment.CheckoutRequest{Reference: "zboard-7", IdempotencyKey: "fixed-key", ReturnURL: "https://panel.example.test/orders/7", AmountMinor: 1200, Currency: "CNY"})
	if err == nil {
		t.Fatal("merchant checkout must not accept a different return destination")
	}
	_, err = client.CreateCheckout(t.Context(), payment.CheckoutRequest{Reference: "zboard-7", IdempotencyKey: "another-key", AmountMinor: 1200, Currency: "CNY"})
	if err == nil {
		t.Fatal("merchant checkout must not accept an unexpected return destination")
	}
}

func TestMerchantPaymentStatesAndErrors(t *testing.T) {
	var state atomic.Value
	state.Store("review")
	var responseCode atomic.Int32
	responseCode.Store(http.StatusCreated)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 3600})
		case "/openapi/v1/merchant-payments":
			w.WriteHeader(int(responseCode.Load()))
			if responseCode.Load() != http.StatusCreated {
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "payment_conflict"})
				return
			}
			_ = json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp123", ExternalRef: "zboard-7", PaymentNo: "stripe-1", AmountMinor: 1200,
				Currency: "CNY", CurrencyDecimals: intPointer(2), Status: state.Load().(string), CheckoutURL: "https://checkout.example.test/1", ExpiresAt: time.Now().Add(-time.Minute)})
		case "/openapi/v1/merchant-payments/mp123":
			_ = json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp123", ExternalRef: "zboard-7", PaymentNo: "stripe-1", AmountMinor: 1200,
				Currency: "CNY", CurrencyDecimals: intPointer(2), Status: state.Load().(string)})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []MerchantChannel{{PaymentNo: "other", Currency: "CNY", CurrencyDecimals: intPointer(2)}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New("tdcloud", server.URL, "client", "secret", map[string]int{"CNY": 2, "USD": 2}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(t.Context()); err != nil {
		t.Fatalf("cashier access must not require a specific directory ID: %v", err)
	}
	// A saved attempt remains usable even when its channel leaves the directory.
	client.amount = &AmountMetadata{Version: 1, Currency: "CNY", Decimals: 2}
	request := payment.CheckoutRequest{Reference: "zboard-7", IdempotencyKey: "fixed-key", AmountMinor: 1200, Currency: "CNY"}
	checkout, err := client.CreateCheckout(t.Context(), request)
	if err != nil || checkout.Status != "review" || checkout.ProviderOrderID != "mp123" {
		t.Fatalf("review order must remain reconcilable: %+v %v", checkout, err)
	}
	result, err := client.Lookup(t.Context(), "mp123")
	if err != nil || result.Status != "unknown" || result.Reference != "zboard-7" {
		t.Fatalf("review cannot settle: %+v %v", result, err)
	}
	state.Store("expired")
	if _, err := client.CreateCheckout(t.Context(), request); !errors.Is(err, payment.ErrCheckoutFailed) {
		t.Fatalf("verified expiry is terminal: %v", err)
	}
	result, err = client.Lookup(t.Context(), "mp123")
	if err != nil || result.Status != "failed" {
		t.Fatalf("expiry result: %+v %v", result, err)
	}
	state.Store("chargeback")
	result, err = client.Lookup(t.Context(), "mp123")
	if err != nil || result.Status != "reversed" {
		t.Fatalf("chargeback cannot settle: %+v %v", result, err)
	}
	responseCode.Store(http.StatusConflict)
	_, err = client.CreateCheckout(t.Context(), request)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || apiErr.Code != "payment_conflict" {
		t.Fatalf("idempotency conflict must be explicit: %v", err)
	}
}
