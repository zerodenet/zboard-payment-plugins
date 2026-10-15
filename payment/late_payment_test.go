package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type receiptHost struct {
	mu           sync.Mutex
	order        Order
	facts        map[string]PaymentFact
	fulfillments int
	loseAck      bool
}

func (h *receiptHost) GetOrder(context.Context, string, uint64) (Order, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.order, nil
}
func (h *receiptHost) Record(_ context.Context, _ string, f PaymentFact) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, found := h.facts[f.ProviderEventID]; found {
		if !sameFact(old, f) {
			return ErrFactNeedsReview
		}
		return nil
	}
	if h.order.Status != "pending" {
		return ErrFactNeedsReview
	}
	h.facts[f.ProviderEventID] = f
	h.order.Status = "paid"
	h.fulfillments++
	if h.loseAck {
		h.loseAck = false
		return errors.New("host response lost")
	}
	return nil
}

type lateChannel struct {
	mu               sync.Mutex
	results          map[string]PaymentResult
	creates, lookups int
}

func (*lateChannel) ID() string { return "test" }
func (*lateChannel) Describe(context.Context) (ChannelInfo, error) {
	return ChannelInfo{Currency: "CNY"}, nil
}
func (c *lateChannel) CreateCheckout(_ context.Context, r CheckoutRequest) (Checkout, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.creates++
	id := fmt.Sprintf("mp-%d", c.creates)
	c.results[id] = PaymentResult{ProviderOrderID: id, Reference: r.Reference, AmountMinor: r.AmountMinor, Currency: r.Currency, Status: "pending", StatusVersion: 1}
	return Checkout{ProviderOrderID: id, RedirectURL: "https://cashier.example.test/" + id, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (c *lateChannel) Lookup(_ context.Context, id string) (PaymentResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lookups++
	r, ok := c.results[id]
	if !ok {
		return PaymentResult{}, ErrNotFound
	}
	return r, nil
}
func (c *lateChannel) state(id, state string, v uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.results[id]
	r.Status, r.StatusVersion = state, v
	if state == "paid" {
		r.EventID, r.ProviderTradeNo, r.OccurredAt = "event-"+id, "trade-"+id, time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
	}
	c.results[id] = r
}

func lateFixture(t *testing.T) (Service, *lateChannel, *receiptHost) {
	t.Helper()
	host := &receiptHost{order: Order{ID: 7, AccountID: "buyer", TradeNo: "zb-7", AmountMinor: 1500, Currency: "CNY", Status: "pending"}, facts: map[string]PaymentFact{}}
	channel := &lateChannel{results: map[string]PaymentResult{}}
	s := Service{Orders: host, Facts: host, Attempts: &testStore{attempts: map[string]Attempt{}}, Channels: map[string]Channel{"test": channel}, ChannelSnapshot: []byte("original credentials")}
	if _, err := s.Start(t.Context(), "buyer", 7, "test", ""); err != nil {
		t.Fatal(err)
	}
	channel.state("mp-1", "failed", 2)
	if _, err := s.Reconcile(t.Context(), "buyer", 7); err != nil {
		t.Fatal(err)
	}
	return s, channel, host
}

func TestExpiredAttemptRecoversPaidThroughAuthenticatedLookup(t *testing.T) {
	s, c, h := lateFixture(t)
	c.state("mp-1", "paid", 3)
	a, err := s.Reconcile(t.Context(), "buyer", 7)
	if err != nil || a.State != "paid" || a.ProviderVersion != 3 || a.FactState != "recorded" || h.fulfillments != 1 {
		t.Fatalf("late payment lost: %+v %v", a, err)
	}
}

func TestRenewedHistoryRetainsTwoReceiptsAndFulfillsBusinessOrderOnce(t *testing.T) {
	s, c, h := lateFixture(t)
	s.PrepareChannel = func(context.Context, Channel, Order) (Channel, []byte, error) {
		return c, []byte("replacement credentials"), nil
	}
	if _, err := s.Start(t.Context(), "buyer", 7, "test", ""); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Attempts.Get(t.Context(), "buyer", 7)
	if len(a.History) != 1 || string(a.History[0].ChannelSnapshot) != "original credentials" || string(a.ChannelSnapshot) != "replacement credentials" {
		t.Fatalf("frozen authority lost: %+v", a)
	}
	c.state("mp-1", "paid", 3)
	c.state("mp-2", "paid", 2)
	a, err := s.Reconcile(t.Context(), "buyer", 7)
	if err != nil || a.History[0].VerifiedFact == nil || a.VerifiedFact == nil || a.History[0].FactState != "recorded" || a.FactState != "review" || a.ReviewReason != "additional_payment" || h.fulfillments != 1 || len(h.facts) != 1 {
		t.Fatalf("duplicate payment lost or fulfilled twice: %+v %v", a, err)
	}
	for range 3 {
		if _, err := s.Reconcile(t.Context(), "buyer", 7); err != nil {
			t.Fatal(err)
		}
	}
	if h.fulfillments != 1 || c.creates != 2 {
		t.Fatal("replay created order or fulfilled again")
	}
}

func TestClosedBusinessOrderLatePaymentIsDurableManualReview(t *testing.T) {
	s, c, h := lateFixture(t)
	h.order.Status = "cancelled"
	c.state("mp-1", "paid", 3)
	a, err := s.Reconcile(t.Context(), "buyer", 7)
	if err != nil || a.VerifiedFact == nil || a.FactState != "review" || a.ReviewReason != "closed_business_order" || h.fulfillments != 0 || len(h.facts) != 0 {
		t.Fatalf("closed-order receipt dropped: %+v %v", a, err)
	}
}

func TestPaidFactAndHostIdempotencySurviveLostAcknowledgementAndRestart(t *testing.T) {
	s, c, h := lateFixture(t)
	c.state("mp-1", "paid", 3)
	h.loseAck = true
	a, err := s.Reconcile(t.Context(), "buyer", 7)
	if err == nil || a.VerifiedFact == nil || a.FactState != "pending" {
		t.Fatalf("unacknowledged fact lost: %+v %v", a, err)
	}
	raw, _ := json.Marshal(a)
	var restored Attempt
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	s.Attempts = &testStore{attempts: map[string]Attempt{"buyer/7": restored}}
	a, err = s.Reconcile(t.Context(), "buyer", 7)
	if err != nil || a.FactState != "recorded" || h.fulfillments != 1 || len(h.facts) != 1 {
		t.Fatalf("restart repeated fulfillment: %+v %v", a, err)
	}
}

func TestConcurrentNotificationLookupAndOlderExpiryCannotRegressPayment(t *testing.T) {
	s, c, h := lateFixture(t)
	c.state("mp-1", "paid", 3)
	failures := make(chan error, 3)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ReconcileProvider(t.Context(), "buyer", 7, "test", "mp-1", 3, true)
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	c.state("mp-1", "failed", 2)
	a, err := s.ReconcileProvider(t.Context(), "buyer", 7, "test", "mp-1", 0, true)
	if err != nil || a.State != "paid" || a.ProviderVersion != 3 || a.VerifiedFact == nil || h.fulfillments != 1 || len(h.facts) != 1 {
		t.Fatalf("older expiry regressed paid: %+v %v", a, err)
	}
}

func TestPeriodicRecoveryChecksExpiredHistoryWithIntervals(t *testing.T) {
	s, c, h := lateFixture(t)
	if _, err := s.Start(t.Context(), "buyer", 7, "test", ""); err != nil {
		t.Fatal(err)
	}
	c.state("mp-1", "paid", 3)
	before := c.lookups
	if _, err := s.ReconcileDue(t.Context(), "buyer", 7, time.Now()); err != nil {
		t.Fatal(err)
	}
	if c.lookups != before+1 || h.fulfillments != 0 {
		t.Fatal("expired history polled without interval")
	}
	a, err := s.ReconcileDue(t.Context(), "buyer", 7, time.Now().Add(25*time.Hour))
	if err != nil || a.SettledProvider != "test:mp-1" || h.fulfillments != 1 {
		t.Fatalf("historical recovery missed late payment: %+v %v", a, err)
	}
}

func TestConcurrentDistinctAttemptReceiptsRemainSeparateAndFulfillOnce(t *testing.T) {
	s, c, h := lateFixture(t)
	if _, err := s.Start(t.Context(), "buyer", 7, "test", ""); err != nil {
		t.Fatal(err)
	}
	c.state("mp-1", "paid", 3)
	c.state("mp-2", "paid", 3)
	start := make(chan struct{})
	failures := make(chan error, 3)
	var wg sync.WaitGroup
	for _, id := range []string{"mp-1", "mp-2", "mp-1"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			_, err := s.ReconcileProvider(t.Context(), "buyer", 7, "test", id, 3, true)
			failures <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	a, _ := s.Attempts.Get(t.Context(), "buyer", 7)
	if a.VerifiedFact == nil || a.History[0].VerifiedFact == nil || a.VerifiedFact.ProviderEventID == a.History[0].VerifiedFact.ProviderEventID || h.fulfillments != 1 || len(h.facts) != 1 {
		t.Fatalf("concurrent attempts lost fact or delivered twice: %+v", a)
	}
	recorded, review := 0, 0
	for _, r := range []Attempt{a, a.History[0]} {
		if r.FactState == "recorded" {
			recorded++
		}
		if r.FactState == "review" {
			review++
		}
	}
	if recorded != 1 || review != 1 {
		t.Fatalf("additional payment lacked explicit resolution: %+v", a)
	}
}
