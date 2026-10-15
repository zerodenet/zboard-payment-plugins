package tdcloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func TestUncertainCreateResponsesNeverReleaseAnAttempt(t *testing.T) {
	for _, item := range []struct {
		http int
		code string
	}{
		{202, "provider_result_pending"}, {403, "login_required"}, {403, "merchant_inactive"}, {422, "provider_checkout_rejected"},
		{422, "merchant_fee_funding_unavailable"}, {422, "channel_unavailable"}, {409, "payment_conflict"}, {500, "server_error"}, {503, "notification_unavailable"},
	} {
		p := newContractFixture(t, false, item.http)
		p.mu.Lock()
		p.firstCode = item.code
		p.rejectBeforeCreate = item.http == 403 || item.http == 422 || item.http == 409
		p.mu.Unlock()
		client, _ := New("tdcloud", p.server.URL, "merchant", "secret", p.server.Client())
		store := &stateContractStore{}
		service := payment.Service{Orders: stateContractOrder{}, Attempts: store, Channels: map[string]payment.Channel{"tdcloud": client}}
		_, err := service.Start(t.Context(), "buyer", 7, "tdcloud", "")
		var api *APIError
		if !errors.As(err, &api) || api.StatusCode != item.http || api.Code != item.code || errors.Is(err, payment.ErrCheckoutFailed) || errors.Is(err, payment.ErrAmountPrecision) {
			t.Fatalf("ambiguous response released attempt: %v", err)
		}
		if store.attempt.State != "creating" || store.attempt.AttemptNumber != 1 {
			t.Fatal("uncertain failure released the saved attempt")
		}
		firstKey := string(p.lastBody()["idempotency_key"])
		// Uncertain server/channel results may follow a committed order; explicit
		// access rejections happen before creation. Both retry with the saved key.
		if view, err := service.Start(t.Context(), "buyer", 7, "tdcloud", ""); err != nil || view.ProviderOrderID != "mp" {
			t.Fatalf("same-key retry lost original order: %+v %v", view, err)
		}
		if string(p.lastBody()["idempotency_key"]) != firstKey || store.attempt.AttemptNumber != 1 {
			t.Fatal("retry generated a different attempt key")
		}
		if errors.Is(&APIError{StatusCode: item.http, Code: item.code}, payment.ErrCheckoutFailed) {
			t.Fatalf("cashier code treated as create failure: %s", item.code)
		}
	}
}

type stateContractOrder struct{}

func (stateContractOrder) GetOrder(context.Context, string, uint64) (payment.Order, error) {
	return payment.Order{ID: 7, AccountID: "buyer", TradeNo: "zb", AmountMinor: 1050, Currency: "USD", Status: "pending"}, nil
}

type stateContractStore struct{ attempt payment.Attempt }

func (s *stateContractStore) Get(context.Context, string, uint64) (payment.Attempt, error) {
	if s.attempt.Revision == 0 {
		return payment.Attempt{}, payment.ErrNotFound
	}
	return s.attempt, nil
}
func (s *stateContractStore) Put(_ context.Context, a payment.Attempt) (payment.Attempt, error) {
	if a.Revision != s.attempt.Revision {
		return payment.Attempt{}, payment.ErrConflict
	}
	a.Revision++
	s.attempt = a
	return a, nil
}

func TestReviewContinuesLookupAndExpiryEndsCheckoutWindow(t *testing.T) {
	var state atomic.Value
	state.Store("review")
	p := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 3600})
			return
		}
		if r.URL.Path != "/openapi/v1/merchant-payments/mp" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp", ExternalRef: "zb", Amount: "10.50", Currency: "USD", Status: state.Load().(string)})
	}))
	defer p.Close()
	client, _ := New("tdcloud", p.URL, "merchant", "secret", p.Client())
	for _, item := range []struct{ remote, mapped string }{{"review", "unknown"}, {"pending", "pending"}, {"expired", "failed"}} {
		state.Store(item.remote)
		result, err := client.Lookup(t.Context(), "mp")
		if err != nil || result.Status != item.mapped || result.ProviderState != item.remote {
			t.Fatalf("wrong status mapping: %+v %v", result, err)
		}
	}
}
