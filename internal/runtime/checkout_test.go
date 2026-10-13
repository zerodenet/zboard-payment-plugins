package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

type checkoutOrder struct{}

func (checkoutOrder) GetOrder(_ context.Context, account string, id uint64) (payment.Order, error) {
	if account != "buyer" || id != 42 {
		return payment.Order{}, payment.ErrNotFound
	}
	return payment.Order{ID: id, AccountID: account, TradeNo: "zb-42", AmountMinor: 1050, Currency: "USD", Status: "pending"}, nil
}

type checkoutAttempts struct {
	mu      sync.Mutex
	attempt payment.Attempt
}

func (s *checkoutAttempts) Get(_ context.Context, account string, id uint64) (payment.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempt.Revision == 0 || s.attempt.AccountID != account || s.attempt.OrderID != id {
		return payment.Attempt{}, payment.ErrNotFound
	}
	return s.attempt, nil
}

func (s *checkoutAttempts) Put(_ context.Context, next payment.Attempt) (payment.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if next.Revision != s.attempt.Revision {
		return payment.Attempt{}, payment.ErrConflict
	}
	next.Revision++
	s.attempt = next
	return next, nil
}

func (s *checkoutAttempts) FindByProviderOrder(_ context.Context, providerType, providerOrderID string) (payment.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if providerType != "tdcloud" || providerOrderID == "" || s.attempt.ProviderOrderID != providerOrderID {
		return payment.Attempt{}, payment.ErrNotFound
	}
	return s.attempt, nil
}

type checkoutFacts struct {
	mu     sync.Mutex
	events map[string]bool
}

func checkoutReturnURL(_ context.Context, account string, id uint64) (string, error) {
	if account != "buyer" || id != 42 {
		return "", payment.ErrNotFound
	}
	return "https://board.example/orders/42", nil
}

func (f *checkoutFacts) Record(_ context.Context, account string, fact payment.PaymentFact) error {
	if account != "buyer" || fact.OrderID != 42 || fact.AmountMinor != 1050 || fact.Currency != "USD" {
		return payment.ErrConflict
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[fact.ProviderEventID] = true
	return nil
}

func TestConfiguredCheckoutUsesTDCloudAndConfirmsOnce(t *testing.T) {
	var paid atomic.Bool
	var creates atomic.Int32
	var oauth atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" && r.Header.Get("Authorization") != "Bearer merchant-token" {
			t.Error("merchant request has no OAuth token")
		}
		switch r.URL.Path {
		case "/oauth/token":
			oauth.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant-token", "expires_in": 3600})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"payment_no": "stripe-usd", "currency": "USD", "currency_decimals": 2}}})
		case "/openapi/v1/merchant-payments":
			creates.Add(1)
			var request struct {
				Reference string `json:"external_reference"`
				ReturnURL string `json:"return_url"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.Reference != "zb-42" || request.ReturnURL != "https://board.example/orders/42" {
				t.Errorf("wrong checkout request: %+v", request)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"order_no": "mp-42", "external_reference": "zb-42", "return_url": request.ReturnURL,
				"payment_no": "stripe-usd", "amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": "pending",
				"checkout_url": "https://pay.example/mp-42", "expires_at": time.Now().Add(time.Hour)})
		case "/openapi/v1/merchant-payments/mp-42":
			status := "pending"
			if paid.Load() {
				status = "paid"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"order_no": "mp-42", "external_reference": "zb-42", "payment_no": "stripe-usd",
				"amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": status, "event_id": "event-42", "provider_trade_no": "trade-42", "occurred_at": time.Now().UTC()})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	s := &Server{httpClient: provider.Client(), config: config{Channels: []channelConfig{{ID: "tdcloud", Name: "TDCloud", Type: "tdcloud", Enabled: true,
		Settings: testTDCloudSettings(provider.URL, "stripe-usd")}}}}
	facts := &checkoutFacts{events: map[string]bool{}}
	checkout, err := s.Checkout(CheckoutPorts{Orders: checkoutOrder{}, Facts: facts, Attempts: &checkoutAttempts{}, OrderReturnURL: checkoutReturnURL})
	if err != nil {
		t.Fatal(err)
	}
	methods, err := checkout.Methods(t.Context(), "USD")
	if err != nil || len(methods) != 1 || methods[0].ID != "tdcloud" {
		t.Fatalf("methods: %+v %v", methods, err)
	}
	started, err := checkout.Start(t.Context(), "buyer", 42, "tdcloud")
	if err != nil || started.State != "pending" || started.RedirectURL != "https://pay.example/mp-42" {
		t.Fatalf("start: %+v %v", started, err)
	}
	started, err = checkout.Start(t.Context(), "buyer", 42, "tdcloud")
	if err != nil || started.State != "pending" || creates.Load() != 1 {
		t.Fatalf("idempotent start: %+v %v, creates=%d", started, err, creates.Load())
	}
	// Removing a method stops new checkout, while an existing attempt retains
	// the provider configuration needed for retry and confirmation.
	cfg, _ := json.Marshal(config{Channels: []channelConfig{}})
	if _, err := s.ApplyConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: cfg}); err != nil {
		t.Fatal(err)
	}
	if resumed, err := checkout.Start(t.Context(), "buyer", 42, "tdcloud"); err != nil || resumed.State != "pending" || creates.Load() != 1 {
		t.Fatalf("removed method must preserve an existing checkout: %+v %v", resumed, err)
	}
	if _, err := checkout.Start(t.Context(), "other-buyer", 99, "tdcloud"); err != payment.ErrNotFound {
		t.Fatalf("unknown order received a return URL: %v", err)
	}
	if pending, err := checkout.Confirm(t.Context(), "buyer", 42); err != nil || pending.State != "pending" {
		t.Fatalf("pending confirmation: %+v %v", pending, err)
	}
	paid.Store(true)
	for range 2 {
		result, err := checkout.Confirm(t.Context(), "buyer", 42)
		if err != nil || result.State != "paid" {
			t.Fatalf("paid confirmation: %+v %v", result, err)
		}
	}
	if len(facts.events) != 1 || !facts.events["tdcloud:event-42"] {
		t.Fatalf("payment fact was not idempotent: %+v", facts.events)
	}
	if oauth.Load() != 1 {
		t.Fatalf("stable channel configuration should reuse merchant OAuth token, calls=%d", oauth.Load())
	}
	raw, _ := json.Marshal(started)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "merchant-token") || strings.Contains(string(raw), "stripe-usd") {
		t.Fatalf("checkout view leaked channel settings: %s", raw)
	}
}

func TestAmbiguousCreateRetriesWithPinnedProviderAfterConfigRemoval(t *testing.T) {
	var creates, directoryCalls atomic.Int32
	var directoryDecimals atomic.Int32
	directoryDecimals.Store(8)
	var returnURLs atomic.Int32
	var firstKey string
	const notifyURL = "https://hooks.example.test/.well-known/zboard-payments/v1/tdcloud-notify"
	const notifySecret = "independent-merchant-notification-secret-123456"
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant-token", "expires_in": 3600})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"payment_no": "stripe-usd", "currency": "USD", "currency_decimals": directoryDecimals.Load()}}})
			directoryCalls.Add(1)
		case "/openapi/v1/merchant-payments":
			var input struct {
				AmountMinor   int64  `json:"amount_minor"`
				Key           string `json:"idempotency_key"`
				ReturnURL     string `json:"return_url"`
				NotifyEnabled bool   `json:"notify_enabled"`
				NotifyURL     string `json:"notify_url"`
				NotifySecret  string `json:"notify_secret"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input.AmountMinor != 1050000000 {
				t.Errorf("changed amount on retry: %d", input.AmountMinor)
			}
			if input.ReturnURL != "https://board.example/orders/42" {
				t.Errorf("changed return URL: %q", input.ReturnURL)
			}
			if !input.NotifyEnabled || input.NotifyURL != notifyURL || input.NotifySecret != notifySecret {
				t.Errorf("changed notification settings on retry: %+v", input)
			}
			if creates.Add(1) == 1 {
				firstKey = input.Key
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if input.Key != firstKey {
				t.Errorf("changed idempotency key: %q != %q", input.Key, firstKey)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"order_no": "mp-42", "external_reference": "zb-42", "return_url": input.ReturnURL,
				"payment_no": "stripe-usd", "amount_minor": 1050000000, "currency": "USD", "currency_decimals": 8, "status": "pending",
				"checkout_url": "https://pay.example/mp-42", "expires_at": time.Now().Add(time.Hour)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	settings, _ := json.Marshal(map[string]any{"currencies": map[string]int{"USD": 8, "CNY": 2, "TDC": 8, "JPY": 0}, "api_url": provider.URL, "client_id": "merchant-client", "client_secret": "merchant-secret",
		"payment_no": "stripe-usd", "notify_url": notifyURL, "notify_secret": notifySecret, "notify_enabled": true})
	s := &Server{httpClient: provider.Client(), config: config{Channels: []channelConfig{{ID: "tdcloud", Name: "TDCloud", Type: "tdcloud", Enabled: true,
		Settings: settings}}}}
	checkout, err := s.Checkout(CheckoutPorts{Orders: checkoutOrder{}, Facts: &checkoutFacts{events: map[string]bool{}}, Attempts: &checkoutAttempts{},
		OrderReturnURL: func(ctx context.Context, account string, id uint64) (string, error) {
			if returnURLs.Add(1) != 1 {
				return "", payment.ErrConflict
			}
			return checkoutReturnURL(ctx, account, id)
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkout.Start(t.Context(), "buyer", 42, "tdcloud"); err == nil {
		t.Fatal("ambiguous create must be retried")
	}
	if _, err := s.ApplyConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: []byte(`{"channels":[]}`)}); err != nil {
		t.Fatal(err)
	}
	directoryDecimals.Store(2)
	// Restart the coordinator with no active configuration; only saved state remains.
	restarted := &Server{httpClient: provider.Client()}
	checkout, err = restarted.Checkout(checkout.ports)
	if err != nil {
		t.Fatal(err)
	}
	result, err := checkout.Start(t.Context(), "buyer", 42, "tdcloud")
	if err != nil || result.State != "pending" || creates.Load() != 2 || returnURLs.Load() != 1 || directoryCalls.Load() != 0 {
		t.Fatalf("pinned retry: %+v %v, creates=%d return_urls=%d", result, err, creates.Load(), returnURLs.Load())
	}
}
