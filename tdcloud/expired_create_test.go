package tdcloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

type expiredCreateFacts struct{ calls int }

func (f *expiredCreateFacts) Record(context.Context, string, payment.PaymentFact) error {
	f.calls++
	return nil
}

func TestIdempotentExpiredCreateRetainsProviderIdentityForLaterPaid(t *testing.T) {
	var paid atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant", "expires_in": 3600})
			return
		}
		view := merchantPayment{OrderNo: "mp", ExternalRef: "zb", Amount: "10.50", Currency: "USD", Status: "expired", StatusVersion: 2, ExpiresAt: time.Now().Add(-time.Minute)}
		if r.Method == http.MethodPost {
			w.WriteHeader(201)
		}
		if paid.Load() {
			view.Status, view.StatusVersion = "paid", 3
			view.EventID, view.ProviderTradeNo, view.OccurredAt = "event-mp", "trade-mp", time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
		}
		json.NewEncoder(w).Encode(view)
	}))
	defer server.Close()
	client, err := New("tdcloud", server.URL, "merchant", "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	store := &stateContractStore{}
	facts := &expiredCreateFacts{}
	service := payment.Service{Orders: stateContractOrder{}, Facts: facts, Attempts: store, Channels: map[string]payment.Channel{"tdcloud": client}}
	a, err := service.Start(t.Context(), "buyer", 7, "tdcloud", "")
	if !errors.Is(err, payment.ErrCheckoutFailed) || a.ProviderOrderID != "mp" || store.attempt.ProviderOrderID != "mp" || a.State != "failed" {
		t.Fatalf("expired idempotent response lost order: %+v %v", a, err)
	}
	paid.Store(true)
	a, err = service.Reconcile(t.Context(), "buyer", 7)
	if err != nil || a.State != "paid" || a.ProviderVersion != 3 || facts.calls != 1 {
		t.Fatalf("late fact after expired creation lost: %+v %v", a, err)
	}
}
