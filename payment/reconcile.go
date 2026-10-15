package payment

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Reconcile includes expired and superseded attempts. Paid facts are durable
// before calling the host, and the host remains the atomic fulfillment owner.
func (s Service) Reconcile(ctx context.Context, account string, order uint64) (Attempt, error) {
	return s.reconcile(ctx, account, order, time.Time{})
}

// ReconcileDue performs bounded periodic recovery, independent of notification
// delivery. Explicit buyer confirmations and signed notifications bypass delay.
func (s Service) ReconcileDue(ctx context.Context, account string, order uint64, now time.Time) (Attempt, error) {
	return s.reconcile(ctx, account, order, now)
}

func (s Service) reconcile(ctx context.Context, account string, order uint64, due time.Time) (Attempt, error) {
	root, err := s.Attempts.Get(ctx, account, order)
	if err != nil {
		return Attempt{}, err
	}
	items := append(append([]Attempt(nil), root.History...), root)
	var first error
	checked := 0
	for _, a := range items {
		if a.ProviderOrderID == "" {
			continue
		}
		if a.State == "paid" && (a.FactState != "pending") {
			continue
		}
		if !due.IsZero() && (a.NextCheckAt.After(due) || checked >= 10) {
			continue
		}
		checked++
		root, err = s.ReconcileProvider(ctx, account, order, a.ChannelID, a.ProviderOrderID, 0, false)
		if err != nil && first == nil {
			first = err
		}
		if ctx.Err() != nil {
			return root, ctx.Err()
		}
	}
	if root.ProviderOrderID == "" && len(root.History) == 0 && first == nil {
		first = ErrPending
	}
	if root.SettledProvider != "" && errors.Is(first, ErrCheckoutExpiryPending) {
		first = nil
	}
	return root, first
}

func (s Service) ReconcileProvider(ctx context.Context, account string, order uint64, channelID, providerID string, minimumVersion uint64, force bool) (Attempt, error) {
	root, err := s.Attempts.Get(ctx, account, order)
	if err != nil {
		return Attempt{}, err
	}
	a, found := FindAttempt(root, channelID, providerID)
	if !found {
		return root, ErrNotFound
	}
	if a.State == "paid" && a.FactState != "pending" && !force {
		return root, nil
	}
	if a.VerifiedFact != nil && a.FactState == "pending" {
		return s.deliverFact(ctx, account, order, channelID, providerID)
	}
	channel := s.Channels[channelID]
	if s.ResolveChannel != nil {
		if len(a.ChannelSnapshot) == 0 {
			return root, ErrHistoricalUnavailable
		}
		channel, err = s.ResolveChannel(a)
		if err != nil {
			return root, err
		}
	}
	if channel == nil || channel.ID() != channelID {
		return root, ErrConflict
	}
	result, err := channel.Lookup(ctx, providerID)
	if err != nil {
		_, saveErr := s.mutate(ctx, account, order, channelID, providerID, func(_ *Attempt, a *Attempt) error { a.NextCheckAt = time.Now().Add(5 * time.Minute); return nil })
		if saveErr != nil {
			return root, saveErr
		}
		return root, err
	}
	if result.ProviderOrderID != providerID || result.Reference != a.OrderTradeNo || result.AmountMinor != a.AmountMinor || !strings.EqualFold(result.Currency, a.Currency) {
		return root, ErrConflict
	}
	if result.StatusVersion != 0 && result.StatusVersion < minimumVersion {
		return root, ErrPending
	}
	root, err = s.mutate(ctx, account, order, channelID, providerID, func(_ *Attempt, a *Attempt) error {
		if result.StatusVersion != 0 && a.ProviderVersion != 0 && result.StatusVersion < a.ProviderVersion {
			return nil
		}
		if result.StatusVersion > a.ProviderVersion {
			a.ProviderVersion = result.StatusVersion
		}
		a.NextCheckAt = time.Now().Add(5 * time.Minute)
		// Payment proof never disappears because an older expiry snapshot arrived.
		if a.VerifiedFact != nil && result.Status != "paid" {
			if result.Status == "reversed" {
				a.FactState, a.ReviewReason = "review", "provider_reversal"
			}
			return nil
		}
		switch result.Status {
		case "pending", "unknown":
			// Do not release an unresolved attempt or revive an expired checkout.
			if a.State != "failed" {
				a.State = "pending"
			}
		case "failed":
			a.State, a.NextCheckAt = "failed", time.Now().Add(24*time.Hour)
		case "reversed":
			a.State, a.ReviewReason, a.NextCheckAt = "review", "provider_reversal", time.Now().Add(24*time.Hour)
		case "paid":
			if result.EventID == "" || result.ProviderTradeNo == "" || result.OccurredAt.IsZero() {
				return ErrConflict
			}
			fact := PaymentFact{OrderID: order, ProviderEventID: channelID + ":" + result.EventID, ProviderTradeNo: result.ProviderTradeNo, Status: "paid", AmountMinor: a.AmountMinor, Currency: a.Currency, OccurredAt: result.OccurredAt}
			if a.VerifiedFact != nil && !sameFact(*a.VerifiedFact, fact) {
				return ErrConflict
			}
			if a.VerifiedFact == nil {
				a.VerifiedFact, a.FactState = &fact, "pending"
			}
		default:
			return ErrConflict
		}
		return nil
	})
	if err != nil {
		return root, err
	}
	updated, _ := FindAttempt(root, channelID, providerID)
	if updated.VerifiedFact != nil {
		return s.deliverFact(ctx, account, order, channelID, providerID)
	}
	if updated.State == "failed" && result.Status != "failed" {
		return root, ErrCheckoutExpiryPending
	}
	if (result.Status == "pending" || result.Status == "unknown") && !updated.ExpiresAt.IsZero() && !updated.ExpiresAt.After(time.Now()) {
		return root, ErrCheckoutExpiryPending
	}
	return root, nil
}

func sameFact(a, b PaymentFact) bool {
	return a.OrderID == b.OrderID && a.ProviderEventID == b.ProviderEventID && a.ProviderTradeNo == b.ProviderTradeNo && a.Status == b.Status && a.AmountMinor == b.AmountMinor && a.Currency == b.Currency && a.OccurredAt.Equal(b.OccurredAt)
}

// mutate reloads the whole order on contention. A lookup racing with renewal
// updates the archived leaf instead of overwriting the new current attempt.
func (s Service) mutate(ctx context.Context, account string, order uint64, channelID, providerID string, change func(*Attempt, *Attempt) error) (Attempt, error) {
	for range 8 {
		root, err := s.Attempts.Get(ctx, account, order)
		if err != nil {
			return root, err
		}
		root = CloneAttempt(root)
		a, found := FindAttempt(root, channelID, providerID)
		if !found {
			return root, ErrNotFound
		}
		if err := change(&root, &a); err != nil {
			return root, err
		}
		if !updateAttempt(&root, a) {
			return root, ErrConflict
		}
		next, err := s.Attempts.Put(ctx, root)
		if errors.Is(err, ErrConflict) {
			continue
		}
		return next, err
	}
	return Attempt{}, ErrConflict
}

func (s Service) deliverFact(ctx context.Context, account string, order uint64, channelID, providerID string) (Attempt, error) {
	root, err := s.Attempts.Get(ctx, account, order)
	if err != nil {
		return root, err
	}
	a, found := FindAttempt(root, channelID, providerID)
	if !found || a.VerifiedFact == nil {
		return root, ErrConflict
	}
	if a.FactState == "recorded" || a.FactState == "review" {
		return root, nil
	}
	reason := ""
	if root.SettledProvider != "" && root.SettledProvider != a.ChannelID+":"+providerID {
		reason = "additional_payment"
	}
	owned, err := s.Orders.GetOrder(ctx, account, order)
	if err != nil {
		return root, err
	}
	if owned.ID != order || owned.AccountID != account || owned.TradeNo != a.OrderTradeNo || owned.AmountMinor != a.AmountMinor || !strings.EqualFold(owned.Currency, a.Currency) {
		return root, ErrConflict
	}
	if owned.Status != "pending" && owned.Status != "paid" {
		reason = "closed_business_order"
	}
	if reason == "" {
		if s.Facts == nil {
			return root, ErrPending
		}
		err = s.Facts.Record(ctx, account, *a.VerifiedFact)
		if errors.Is(err, ErrFactNeedsReview) {
			reason = "host_payment_conflict"
		} else if err != nil {
			return root, err
		}
	}
	return s.mutate(ctx, account, order, channelID, providerID, func(root *Attempt, a *Attempt) error {
		if a.FactState == "recorded" || a.FactState == "review" {
			return nil
		}
		if reason != "" {
			a.State, a.FactState, a.ReviewReason = "paid", "review", reason
		} else {
			a.State, a.FactState = "paid", "recorded"
			root.SettledProvider = a.ChannelID + ":" + providerID
		}
		return nil
	})
}
