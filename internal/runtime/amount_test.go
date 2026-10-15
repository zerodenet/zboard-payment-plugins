package runtime

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

func TestProviderRejectsDecimalPrecisionWithoutLockingAnotherPaymentOption(t *testing.T) {
	var creates atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": r.FormValue("client_id"), "expires_in": 300})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
				{"payment_no": "whole", "currency": "USD", "currency_decimals": 0},
				{"payment_no": "cents", "currency": "USD", "currency_decimals": 2},
			}})
		case "/openapi/v1/merchant-payments":
			var input map[string]any
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input["payment_no"] != nil || input["amount_minor"] != nil || input["amount"] != "10.50" {
				t.Errorf("unexpected provider creation: %+v", input)
			}
			if r.Header.Get("Authorization") == "Bearer whole" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_amount_precision"})
				return
			}
			creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"order_no": "mp", "external_reference": "zb-42", "payment_no": "cents", "amount": "10.50", "amount_minor": 1050,
				"currency": "USD", "currency_decimals": 2, "return_url": input["return_url"], "status": "pending", "checkout_url": "https://pay.example/mp", "expires_at": time.Now().Add(time.Hour)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	s := &Server{httpClient: provider.Client()}
	for _, id := range []string{"whole", "cents"} {
		settings, _ := json.Marshal(map[string]any{"api_url": provider.URL, "client_id": id, "client_secret": "secret", "payment_no": id})
		s.config.Channels = append(s.config.Channels, channelConfig{ID: id, Type: "tdcloud", Enabled: true, Settings: settings})
	}
	attempts := &checkoutAttempts{}
	checkout, _ := s.Checkout(CheckoutPorts{Orders: checkoutOrder{}, Facts: &checkoutFacts{}, Attempts: attempts, OrderReturnURL: checkoutReturnURL})
	if _, err := checkout.Start(t.Context(), "buyer", 42, "whole"); !errors.Is(err, payment.ErrAmountPrecision) || creates.Load() != 0 {
		t.Fatalf("formal precision rejection did not release attempt: %v creates=%d", err, creates.Load())
	}
	saved, _ := attempts.Get(t.Context(), "buyer", 42)
	if saved.State != "failed" || saved.ProviderOrderID != "" {
		t.Fatalf("precision rejection left checkout locked: %+v", saved)
	}
	if result, err := checkout.Start(t.Context(), "buyer", 42, "cents"); err != nil || result.State != "pending" || creates.Load() != 1 {
		t.Fatalf("could not select another method: %+v %v", result, err)
	}
}
