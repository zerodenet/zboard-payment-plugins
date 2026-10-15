package payment

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

type expiryChannel struct {
	*testChannel
	lookupErr error
	lookups   int
}

func (c *expiryChannel) Lookup(context.Context, string) (PaymentResult, error) {
	c.lookups++
	return c.result, c.lookupErr
}

func (c *expiryChannel) CreateCheckout(ctx context.Context, in CheckoutRequest) (Checkout, error) {
	checkout, err := c.testChannel.CreateCheckout(ctx, in)
	checkout.ProviderOrderID = "provider-" + strconv.Itoa(c.creates)
	return checkout, err
}

func expiredFixture(t *testing.T) (Service, *expiryChannel, *testFacts) {
	t.Helper()
	s, base, facts := fixture()
	channel := &expiryChannel{testChannel: base}
	s.Channels["test"] = channel
	attempt, err := s.Start(t.Context(), "account-1", 7, "test", "https://panel.example.test/orders/7")
	if err != nil {
		t.Fatal(err)
	}
	attempt.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := s.Attempts.Put(t.Context(), attempt); err != nil {
		t.Fatal(err)
	}
	channel.result = PaymentResult{ProviderOrderID: "provider-1", Reference: "zboard-7", AmountMinor: 1200, Currency: "CNY"}
	return s, channel, facts
}

func TestExpiredCheckoutRechecksAndReplacesOnlyConfirmedFailure(t *testing.T) {
	s, channel, facts := expiredFixture(t)
	channel.result.Status = "failed"
	retried, err := s.Start(t.Context(), "account-1", 7, "test", "")
	if err != nil || retried.OrderID != 7 || retried.ProviderOrderID != "provider-2" || retried.AttemptNumber != 2 ||
		channel.lookups != 1 || channel.creates != 2 || !retried.ExpiresAt.After(time.Now()) || len(facts.calls) != 0 {
		t.Fatalf("terminal expiry did not renew checkout: %+v %v", retried, err)
	}
	if len(retried.PreviousOrders) != 1 || retried.PreviousOrders[0] != "provider-1" ||
		channel.keys[0] == channel.keys[1] || channel.returnURLs[0] != channel.returnURLs[1] {
		t.Fatalf("renewal changed business identity or lost history: %+v", retried)
	}
	// A subsequent click resumes the fresh checkout, without another query/create.
	if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); err != nil || channel.creates != 2 || channel.lookups != 1 {
		t.Fatalf("fresh attempt duplicated: %v", err)
	}
}

func TestExpiredCheckoutNeverReopensOrRecreatesUnconfirmedPayment(t *testing.T) {
	for _, state := range []string{"pending", "unknown"} {
		t.Run(state, func(t *testing.T) {
			s, channel, facts := expiredFixture(t)
			channel.result.Status = state
			for range 2 {
				if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); !errors.Is(err, ErrCheckoutExpiryPending) {
					t.Fatalf("unconfirmed expiry should stop payment: %v", err)
				}
			}
			if _, err := s.Reconcile(t.Context(), "account-1", 7); !errors.Is(err, ErrCheckoutExpiryPending) {
				t.Fatalf("confirmation should explain expiry: %v", err)
			}
			stored, _ := s.Attempts.Get(t.Context(), "account-1", 7)
			if stored.AttemptNumber != 1 || stored.State != "pending" || channel.creates != 1 || len(facts.calls) != 0 {
				t.Fatalf("deadline alone released payment: %+v", stored)
			}
		})
	}
}

func TestExpiredCheckoutLookupFailureOrIdentityMismatchDoesNotReleaseAttempt(t *testing.T) {
	for _, reason := range []string{"network", "reference", "amount", "currency"} {
		t.Run(reason, func(t *testing.T) {
			s, channel, facts := expiredFixture(t)
			channel.result.Status = "failed"
			switch reason {
			case "network":
				channel.lookupErr = errors.New("provider unavailable")
			case "reference":
				channel.result.Reference = "other"
			case "amount":
				channel.result.AmountMinor++
			case "currency":
				channel.result.Currency = "USD"
			}
			if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); err == nil {
				t.Fatal("unverified expiry released checkout")
			}
			stored, _ := s.Attempts.Get(t.Context(), "account-1", 7)
			if stored.State != "pending" || stored.AttemptNumber != 1 || channel.creates != 1 || len(facts.calls) != 0 {
				t.Fatalf("attempt replaced: %+v", stored)
			}
		})
	}
}

func TestExpiredCheckoutLatePaymentSettlesWithoutCreatingAnotherOrder(t *testing.T) {
	s, channel, facts := expiredFixture(t)
	channel.result.Status = "paid"
	channel.result.EventID, channel.result.ProviderTradeNo = "event-1", "trade-1"
	channel.result.OccurredAt = time.Now()
	paid, err := s.Start(t.Context(), "account-1", 7, "test", "")
	if err != nil || paid.State != "paid" || paid.RedirectURL != "" || channel.creates != 1 || len(facts.calls) != 1 {
		t.Fatalf("late payment reopened or recreated cashier: %+v %v", paid, err)
	}
	if _, err := s.Reconcile(t.Context(), "account-1", 7); err != nil || len(facts.calls) != 1 {
		t.Fatalf("late payment repeated fact: %v", err)
	}
}

func TestExpiredCheckoutRenewalRequiresFreshChannelPreparation(t *testing.T) {
	s, channel, _ := expiredFixture(t)
	channel.result.Status = "failed"
	s.PrepareChannel = func(context.Context, Channel, Order) (Channel, []byte, error) {
		return nil, nil, ErrConflict // the host configuration disabled this channel
	}
	if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("disabled channel renewed: %v", err)
	}
	stored, _ := s.Attempts.Get(t.Context(), "account-1", 7)
	if stored.State != "failed" || stored.AttemptNumber != 1 || channel.creates != 1 {
		t.Fatalf("old snapshot was used to create a new checkout: %+v", stored)
	}
}
