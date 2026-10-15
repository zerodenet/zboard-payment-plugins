package tdcloud_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	"github.com/zerodenet/zboard-pay-plugins/tdcloud"
)

type flowOrder struct{}

func (flowOrder) GetOrder(_ context.Context, account string, id uint64) (payment.Order, error) {
	if account != "buyer-1" || id != 42 {
		return payment.Order{}, payment.ErrNotFound
	}
	return payment.Order{ID: 42, TradeNo: "zb-42", AccountID: account, AmountMinor: 1050, Currency: "USD", Status: "pending"}, nil
}

type flowAttempts struct {
	mu      sync.Mutex
	attempt payment.Attempt
}

func (s *flowAttempts) Get(_ context.Context, account string, id uint64) (payment.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempt.Revision == 0 || account != "buyer-1" || id != 42 {
		return payment.Attempt{}, payment.ErrNotFound
	}
	return s.attempt, nil
}

func (s *flowAttempts) Put(_ context.Context, next payment.Attempt) (payment.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if next.Revision != s.attempt.Revision {
		return payment.Attempt{}, payment.ErrConflict
	}
	next.Revision++
	s.attempt = next
	return next, nil
}

type flowFacts struct {
	mu           sync.Mutex
	events       map[string]bool
	fulfillments int
}

func (f *flowFacts) Record(_ context.Context, account string, fact payment.PaymentFact) error {
	if account != "buyer-1" || fact.OrderID != 42 || fact.AmountMinor != 1050 || fact.Currency != "USD" {
		return payment.ErrConflict
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.events[fact.ProviderEventID] {
		f.events[fact.ProviderEventID] = true
		f.fulfillments++
	}
	return nil
}

func TestTDCloudCheckoutAndRacingConfirmationsFulfillOnce(t *testing.T) {
	var paid atomic.Bool
	var creates atomic.Int32
	occurred := time.Now().UTC().Truncate(time.Second)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant", "expires_in": 3600})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"payment_no": "stripe-usd", "currency": "USD", "currency_decimals": 2}}})
		case "/openapi/v1/merchant-payments":
			creates.Add(1)
			var input struct {
				ReturnURL string `json:"return_url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input.ReturnURL != "https://panel.example.test/orders/42" {
				t.Errorf("missing merchant return URL: %q", input.ReturnURL)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"order_no": "mp-42", "external_reference": "zb-42", "return_url": input.ReturnURL,
				"payment_no": "stripe-usd", "amount": "10.50", "amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": "pending",
				"checkout_url": "https://checkout.example.test/mp-42", "expires_at": time.Now().Add(time.Hour)})
		case "/openapi/v1/merchant-payments/mp-42":
			status := "pending"
			if paid.Load() {
				status = "paid"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"order_no": "mp-42", "external_reference": "zb-42", "payment_no": "stripe-usd",
				"amount": "10.50", "amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": status, "event_id": "event-42", "provider_trade_no": "trade-42", "occurred_at": occurred})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	channel, err := tdcloud.New("tdcloud", provider.URL, "client", "secret", provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	facts := &flowFacts{events: map[string]bool{}}
	service := payment.Service{Orders: flowOrder{}, Attempts: &flowAttempts{}, Facts: facts, Channels: map[string]payment.Channel{"tdcloud": channel}}
	attempt, err := service.Start(t.Context(), "buyer-1", 42, "tdcloud", "https://panel.example.test/orders/42")
	if err != nil || attempt.ProviderOrderID != "mp-42" || attempt.RedirectURL == "" || creates.Load() != 1 {
		t.Fatalf("start checkout: %+v %v, creates=%d", attempt, err, creates.Load())
	}
	if pending, err := service.Reconcile(t.Context(), "buyer-1", 42); err != nil || pending.State != "pending" || facts.fulfillments != 0 {
		t.Fatalf("pending confirmation: %+v %v", pending, err)
	}
	paid.Store(true)
	type result struct {
		attempt payment.Attempt
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			value, err := service.Reconcile(t.Context(), "buyer-1", 42)
			results <- result{value, err}
		}()
	}
	for range 2 {
		value := <-results
		if value.err != nil || value.attempt.State != "paid" {
			t.Fatalf("racing confirmation: %+v", value)
		}
	}
	if facts.fulfillments != 1 {
		t.Fatalf("business order fulfilled %d times", facts.fulfillments)
	}
}
