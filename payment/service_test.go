package payment

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

type testOrders struct{ order Order }

func (s testOrders) GetOrder(_ context.Context, account string, id uint64) (Order, error) {
	if account != s.order.AccountID || id != s.order.ID {
		return Order{}, ErrNotFound
	}
	return s.order, nil
}

type testStore struct {
	mu       sync.Mutex
	attempts map[string]Attempt
}

func stringID(id uint64) string { return strconv.FormatUint(id, 10) }

func (s *testStore) Get(_ context.Context, account string, id uint64) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[account+"/"+stringID(id)]
	if !ok {
		return Attempt{}, ErrNotFound
	}
	return a, nil
}

func (s *testStore) Put(_ context.Context, a Attempt) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := a.AccountID + "/" + stringID(a.OrderID)
	current := s.attempts[key]
	if current.Revision != a.Revision || (current.Revision != 0 && current.ChannelID != a.ChannelID) {
		return Attempt{}, ErrConflict
	}
	a.Revision++
	s.attempts[key] = a
	return a, nil
}

type testChannel struct {
	creates        int
	result         PaymentResult
	failOnce       bool
	transientOnce  bool
	keys           []string
	returnURLs     []string
	checkoutStatus string
}

func (*testChannel) ID() string { return "test" }
func (*testChannel) Describe(context.Context) (ChannelInfo, error) {
	return ChannelInfo{Currency: "CNY"}, nil
}
func (c *testChannel) CreateCheckout(_ context.Context, request CheckoutRequest) (Checkout, error) {
	if request.IdempotencyKey == "" || request.AmountMinor != 1200 || request.Currency != "CNY" {
		return Checkout{}, ErrConflict
	}
	c.creates++
	c.keys = append(c.keys, request.IdempotencyKey)
	c.returnURLs = append(c.returnURLs, request.ReturnURL)
	if c.transientOnce {
		c.transientOnce = false
		return Checkout{}, errors.New("temporary network failure")
	}
	if c.failOnce {
		c.failOnce = false
		return Checkout{}, ErrCheckoutFailed
	}
	if c.checkoutStatus == "paid" || c.checkoutStatus == "review" {
		return Checkout{ProviderOrderID: "provider-1", Status: c.checkoutStatus, ExpiresAt: time.Now().Add(-time.Minute)}, nil
	}
	return Checkout{ProviderOrderID: "provider-1", RedirectURL: "https://pay.example.test/1", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (c *testChannel) Lookup(context.Context, string) (PaymentResult, error) { return c.result, nil }

type testFacts struct {
	mu            sync.Mutex
	calls         []PaymentFact
	fail          bool
	recordBarrier chan struct{}
}

func (f *testFacts) Record(ctx context.Context, account string, fact PaymentFact) error {
	if account != "account-1" {
		return ErrConflict
	}
	f.mu.Lock()
	f.calls = append(f.calls, fact)
	failed := f.fail
	barrier := f.recordBarrier
	if barrier != nil && len(f.calls) == 2 {
		close(barrier)
	}
	f.mu.Unlock()
	if barrier != nil {
		select {
		case <-barrier:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if failed {
		return ErrConflict
	}
	return nil
}

func fixture() (Service, *testChannel, *testFacts) {
	channel, facts := &testChannel{}, &testFacts{}
	return Service{Orders: testOrders{Order{ID: 7, TradeNo: "zboard-7", AccountID: "account-1", AmountMinor: 1200, Currency: "CNY", Status: "pending"}},
		Facts: facts, Attempts: &testStore{attempts: map[string]Attempt{}}, Channels: map[string]Channel{"test": channel}}, channel, facts
}

func TestStartReusesCheckoutAndRejectsDifferentChannel(t *testing.T) {
	s, channel, _ := fixture()
	first, err := s.Start(t.Context(), "account-1", 7, "test", "https://panel.example.test/orders/7")
	if err != nil || first.ProviderOrderID != "provider-1" {
		t.Fatalf("first checkout: %+v, %v", first, err)
	}
	second, err := s.Start(t.Context(), "account-1", 7, "test", "")
	if err != nil || second.ProviderOrderID != first.ProviderOrderID || channel.creates != 1 {
		t.Fatalf("reused checkout: %+v, creates=%d, err=%v", second, channel.creates, err)
	}
	if _, err := s.Start(t.Context(), "account-1", 7, "other", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("different channel: %v", err)
	}
	if _, err := s.Start(t.Context(), "account-1", 7, "test", "https://wrong.example.test/orders/7"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different return URL must not reuse an existing checkout: %v", err)
	}
}

func TestTransientCreateRetryPreservesTDCloudReturnURLAndIdempotencyKey(t *testing.T) {
	s, channel, _ := fixture()
	channel.transientOnce = true
	returnURL := "https://panel.example.test/orders/7?from=tdcloud#receipt"
	if _, err := s.Start(t.Context(), "account-1", 7, "test", returnURL); err == nil {
		t.Fatal("transient create failure should be reported")
	}
	attempt, err := s.Start(t.Context(), "account-1", 7, "test", "")
	if err != nil || attempt.State != "pending" || attempt.ReturnURL != returnURL || len(channel.keys) != 2 || channel.keys[0] != channel.keys[1] || channel.returnURLs[0] != returnURL || channel.returnURLs[1] != returnURL {
		t.Fatalf("retry changed provider request: %+v %v, keys=%v urls=%v", attempt, err, channel.keys, channel.returnURLs)
	}
}

func TestReconcileRequiresMatchingProviderFactAndRecordsOnce(t *testing.T) {
	s, channel, facts := fixture()
	if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); err != nil {
		t.Fatal(err)
	}
	channel.result = PaymentResult{ProviderOrderID: "provider-1", Reference: "zboard-7", ProviderTradeNo: "trade-1", EventID: "event-1", Status: "paid", AmountMinor: 1201, Currency: "CNY", OccurredAt: time.Now()}
	if _, err := s.Reconcile(t.Context(), "account-1", 7); !errors.Is(err, ErrConflict) || len(facts.calls) != 0 {
		t.Fatalf("mismatched amount was recorded: %v, %+v", err, facts.calls)
	}
	channel.result.AmountMinor = 1200
	attempt, err := s.Reconcile(t.Context(), "account-1", 7)
	if err != nil || attempt.State != "paid" || len(facts.calls) != 1 || facts.calls[0].ProviderEventID != "test:event-1" {
		t.Fatalf("paid reconcile: %+v, %v, %+v", attempt, err, facts.calls)
	}
	if _, err := s.Reconcile(t.Context(), "account-1", 7); err != nil || len(facts.calls) != 1 {
		t.Fatalf("duplicate reconcile: %v, %+v", err, facts.calls)
	}
}

func TestConcurrentConfirmationReturnsOnePaidResult(t *testing.T) {
	s, channel, facts := fixture()
	if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); err != nil {
		t.Fatal(err)
	}
	channel.result = PaymentResult{ProviderOrderID: "provider-1", Reference: "zboard-7", ProviderTradeNo: "trade-1", EventID: "event-1",
		Status: "paid", AmountMinor: 1200, Currency: "CNY", OccurredAt: time.Now()}
	facts.recordBarrier = make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	type outcome struct {
		attempt Attempt
		err     error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			attempt, err := s.Reconcile(ctx, "account-1", 7)
			results <- outcome{attempt: attempt, err: err}
		}()
	}
	for range 2 {
		result := <-results
		if result.err != nil || result.attempt.State != "paid" {
			t.Fatalf("concurrent confirmation: %+v", result)
		}
	}
	if len(facts.calls) != 2 || facts.calls[0].ProviderEventID != facts.calls[1].ProviderEventID {
		t.Fatalf("racing confirmations must reuse one event: %+v", facts.calls)
	}
}

func TestUnknownResultNeverSettles(t *testing.T) {
	s, channel, facts := fixture()
	if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); err != nil {
		t.Fatal(err)
	}
	channel.result = PaymentResult{ProviderOrderID: "provider-1", Reference: "zboard-7", Status: "unknown", AmountMinor: 1200, Currency: "CNY"}
	attempt, err := s.Reconcile(t.Context(), "account-1", 7)
	if err != nil || attempt.State != "pending" || len(facts.calls) != 0 {
		t.Fatalf("unknown result settled: %+v %v %+v", attempt, err, facts.calls)
	}
}

func TestPaidResultRetriesZBoardSettlementBeforeLocalPaidMarker(t *testing.T) {
	s, channel, facts := fixture()
	if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); err != nil {
		t.Fatal(err)
	}
	channel.result = PaymentResult{ProviderOrderID: "provider-1", Reference: "zboard-7", ProviderTradeNo: "trade-1", EventID: "event-1",
		Status: "paid", AmountMinor: 1200, Currency: "CNY", OccurredAt: time.Now()}
	facts.fail = true
	attempt, err := s.Reconcile(t.Context(), "account-1", 7)
	if !errors.Is(err, ErrConflict) || attempt.State != "pending" {
		t.Fatalf("failed settlement must remain pending: %+v %v", attempt, err)
	}
	facts.fail = false
	attempt, err = s.Reconcile(t.Context(), "account-1", 7)
	if err != nil || attempt.State != "paid" || len(facts.calls) != 2 || facts.calls[0].ProviderEventID != facts.calls[1].ProviderEventID {
		t.Fatalf("settlement retry: %+v %v %+v", attempt, err, facts.calls)
	}
}

func TestFailedProviderCheckoutCanCreateNewIdempotentAttempt(t *testing.T) {
	s, channel, _ := fixture()
	channel.failOnce = true
	if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); !errors.Is(err, ErrCheckoutFailed) {
		t.Fatalf("first failed checkout: %v", err)
	}
	failed, err := s.Attempts.Get(t.Context(), "account-1", 7)
	if err != nil || failed.State != "failed" {
		t.Fatalf("terminal failure not persisted: %+v %v", failed, err)
	}
	second, err := s.Start(t.Context(), "account-1", 7, "test", "")
	if err != nil || second.State != "pending" || len(channel.keys) != 2 || channel.keys[0] == channel.keys[1] {
		t.Fatalf("retry must use a new key: %+v %v %+v", second, err, channel.keys)
	}
}

func TestReconcileRejectsDifferentExternalReference(t *testing.T) {
	s, channel, facts := fixture()
	if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); err != nil {
		t.Fatal(err)
	}
	channel.result = PaymentResult{ProviderOrderID: "provider-1", Reference: "other-order", ProviderTradeNo: "trade-1", EventID: "event-1",
		Status: "paid", AmountMinor: 1200, Currency: "CNY", OccurredAt: time.Now()}
	if _, err := s.Reconcile(t.Context(), "account-1", 7); !errors.Is(err, ErrConflict) || len(facts.calls) != 0 {
		t.Fatalf("different order must never settle: %v, %+v", err, facts.calls)
	}
}

func TestReconcileRejectsMissingExternalReference(t *testing.T) {
	s, channel, facts := fixture()
	if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); err != nil {
		t.Fatal(err)
	}
	channel.result = PaymentResult{ProviderOrderID: "provider-1", ProviderTradeNo: "trade-1", EventID: "event-1",
		Status: "paid", AmountMinor: 1200, Currency: "CNY", OccurredAt: time.Now()}
	if _, err := s.Reconcile(t.Context(), "account-1", 7); !errors.Is(err, ErrConflict) || len(facts.calls) != 0 {
		t.Fatalf("missing external reference must never settle: %v, %+v", err, facts.calls)
	}
}

func TestIdempotentCreateAlreadyPaidStillReconciles(t *testing.T) {
	s, channel, facts := fixture()
	channel.checkoutStatus = "paid"
	attempt, err := s.Start(t.Context(), "account-1", 7, "test", "")
	if err != nil || attempt.ProviderOrderID != "provider-1" || attempt.RedirectURL != "" || attempt.State != "pending" {
		t.Fatalf("paid retry must retain provider order for lookup: %+v %v", attempt, err)
	}
	channel.result = PaymentResult{ProviderOrderID: "provider-1", Reference: "zboard-7", ProviderTradeNo: "trade-1", EventID: "event-1",
		Status: "paid", AmountMinor: 1200, Currency: "CNY", OccurredAt: time.Now()}
	attempt, err = s.Reconcile(t.Context(), "account-1", 7)
	if err != nil || attempt.State != "paid" || len(facts.calls) != 1 {
		t.Fatalf("recovered paid order must settle once: %+v %v %+v", attempt, err, facts.calls)
	}
}

func TestReversedPaymentRequiresReviewWithoutSettlement(t *testing.T) {
	s, channel, facts := fixture()
	if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); err != nil {
		t.Fatal(err)
	}
	channel.result = PaymentResult{ProviderOrderID: "provider-1", Reference: "zboard-7", Status: "reversed", AmountMinor: 1200, Currency: "CNY"}
	attempt, err := s.Reconcile(t.Context(), "account-1", 7)
	if err != nil || attempt.State != "review" || len(facts.calls) != 0 {
		t.Fatalf("reversed payment cannot settle: %+v %v %+v", attempt, err, facts.calls)
	}
	if _, err := s.Start(t.Context(), "account-1", 7, "test", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("review order cannot create another checkout: %v", err)
	}
}
