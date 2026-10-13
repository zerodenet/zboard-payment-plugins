package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

type Service struct {
	Orders          OrderSource
	Facts           FactRecorder
	Attempts        AttemptStore
	Channels        map[string]Channel
	ChannelSnapshot []byte
	ResolveChannel  func(Attempt) (Channel, error)
	PrepareChannel  func(context.Context, Channel, Order) (Channel, []byte, error)
}

func (s Service) Start(ctx context.Context, accountID string, orderID uint64, channelID, returnURL string) (Attempt, error) {
	channel := s.Channels[channelID]
	if accountID == "" || orderID == 0 || channel == nil || channel.ID() != channelID {
		return Attempt{}, ErrConflict
	}
	order, err := s.Orders.GetOrder(ctx, accountID, orderID)
	if err != nil {
		return Attempt{}, err
	}
	if order.ID != orderID || order.AccountID != accountID || order.Status != "pending" || order.AmountMinor <= 0 || len(order.Currency) != 3 || order.TradeNo == "" {
		return Attempt{}, ErrConflict
	}
	if returnURL != "" && !validHTTPSURL(returnURL) {
		return Attempt{}, ErrConflict
	}
	attempt, err := s.Attempts.Get(ctx, accountID, orderID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Attempt{}, err
	}
	if errors.Is(err, ErrNotFound) {
		if s.PrepareChannel != nil {
			channel, s.ChannelSnapshot, err = s.PrepareChannel(ctx, channel, order)
			if err != nil {
				return Attempt{}, err
			}
		}
		attempt = Attempt{AccountID: accountID, OrderID: orderID, OrderTradeNo: order.TradeNo,
			ChannelID: channelID, AmountMinor: order.AmountMinor, Currency: strings.ToUpper(order.Currency), ReturnURL: returnURL,
			ChannelSnapshot: append([]byte(nil), s.ChannelSnapshot...), State: "creating", AttemptNumber: 1}
		attempt, err = s.Attempts.Put(ctx, attempt)
		if errors.Is(err, ErrConflict) {
			attempt, err = s.Attempts.Get(ctx, accountID, orderID)
		}
		if err != nil {
			return Attempt{}, err
		}
	}
	if attempt.OrderTradeNo != order.TradeNo || attempt.AmountMinor != order.AmountMinor || attempt.Currency != strings.ToUpper(order.Currency) {
		return Attempt{}, ErrConflict
	}
	if returnURL != "" && returnURL != attempt.ReturnURL && attempt.State != "failed" {
		return Attempt{}, ErrConflict
	}
	if attempt.State == "failed" {
		if s.PrepareChannel != nil {
			channel, s.ChannelSnapshot, err = s.PrepareChannel(ctx, channel, order)
			if err != nil {
				return Attempt{}, err
			}
		}
		if returnURL != "" {
			attempt.ReturnURL = returnURL
		}
		attempt.PreviousOrders = append(attempt.PreviousOrders, attempt.ProviderOrderID)
		attempt.ChannelID, attempt.ProviderOrderID, attempt.RedirectURL = channelID, "", ""
		attempt.ChannelSnapshot = append([]byte(nil), s.ChannelSnapshot...)
		attempt.ExpiresAt, attempt.State, attempt.AttemptNumber = time.Time{}, "creating", attempt.AttemptNumber+1
		attempt, err = s.Attempts.Put(ctx, attempt)
		if err != nil {
			return Attempt{}, err
		}
	}
	if attempt.ChannelID != channelID {
		return Attempt{}, ErrConflict
	}
	if attempt.State == "review" {
		return Attempt{}, ErrConflict
	}
	if attempt.ProviderOrderID != "" {
		return attempt, nil
	}
	if attempt.State != "creating" {
		return Attempt{}, ErrConflict
	}
	if s.ResolveChannel != nil && len(attempt.ChannelSnapshot) > 0 {
		channel, err = s.ResolveChannel(attempt)
		if err != nil || channel == nil || channel.ID() != channelID {
			return Attempt{}, ErrConflict
		}
	}
	// Every retry uses the same key. Channels without idempotent creation must
	// refuse to implement CreateCheckout rather than risk charging twice.
	checkout, err := channel.CreateCheckout(ctx, CheckoutRequest{
		Reference: order.TradeNo, AmountMinor: order.AmountMinor, Currency: attempt.Currency,
		Description: order.Description, ReturnURL: attempt.ReturnURL,
		IdempotencyKey: idempotencyKey(attempt),
	})
	if errors.Is(err, ErrCheckoutFailed) || errors.Is(err, ErrAmountPrecision) {
		attempt.State = "failed"
		if _, saveErr := s.Attempts.Put(ctx, attempt); saveErr != nil {
			return Attempt{}, saveErr
		}
		return Attempt{}, err
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("create checkout: %w", err)
	}
	if checkout.ProviderOrderID == "" || (checkout.Status != "" && checkout.Status != "pending" && checkout.Status != "review" && checkout.Status != "paid") ||
		((checkout.Status == "" || checkout.Status == "pending") && (!validHTTPSURL(checkout.RedirectURL) || !checkout.ExpiresAt.After(time.Now()))) {
		return Attempt{}, errors.New("channel returned an invalid checkout")
	}
	attempt.ProviderOrderID, attempt.State = checkout.ProviderOrderID, "pending"
	if checkout.Status == "" || checkout.Status == "pending" {
		attempt.RedirectURL, attempt.ExpiresAt = checkout.RedirectURL, checkout.ExpiresAt
	}
	attempt, err = s.Attempts.Put(ctx, attempt)
	if errors.Is(err, ErrConflict) {
		current, getErr := s.Attempts.Get(ctx, accountID, orderID)
		if getErr == nil && current.ProviderOrderID == checkout.ProviderOrderID && current.ChannelID == channelID {
			return current, nil
		}
	}
	return attempt, err
}

// Reconcile handles both the buyer's confirmation button and the returned
// checkout window. It only commits a verified provider fact. A successful
// ZBoard record precedes the local paid marker, so a crash can safely replay
// the same event without fulfilling the business order twice.
func (s Service) Reconcile(ctx context.Context, accountID string, orderID uint64) (Attempt, error) {
	attempt, err := s.Attempts.Get(ctx, accountID, orderID)
	if err != nil {
		return Attempt{}, err
	}
	if attempt.State == "paid" || attempt.State == "failed" || attempt.State == "review" {
		return attempt, nil
	}
	if attempt.ProviderOrderID == "" {
		return attempt, ErrPending
	}
	channel := s.Channels[attempt.ChannelID]
	if s.ResolveChannel != nil && len(attempt.ChannelSnapshot) > 0 {
		channel, err = s.ResolveChannel(attempt)
		if err != nil {
			return attempt, err
		}
	}
	if channel == nil || channel.ID() != attempt.ChannelID {
		return attempt, ErrConflict
	}
	result, err := channel.Lookup(ctx, attempt.ProviderOrderID)
	if err != nil {
		return attempt, err
	}
	if result.ProviderOrderID != attempt.ProviderOrderID || result.Reference != attempt.OrderTradeNo ||
		result.AmountMinor != attempt.AmountMinor || strings.ToUpper(result.Currency) != attempt.Currency {
		return attempt, ErrConflict
	}
	switch result.Status {
	case "pending", "unknown":
		return attempt, nil
	case "failed":
		// A failed provider attempt does not fail the ZBoard business order; a
		// buyer may choose another channel after this terminal attempt.
		attempt.State = "failed"
		return s.Attempts.Put(ctx, attempt)
	case "reversed":
		// A refund or chargeback needs a separate business reversal decision.
		// Keep this attempt out of both settlement and new checkout creation.
		attempt.State = "review"
		return s.Attempts.Put(ctx, attempt)
	case "paid":
		if result.EventID == "" || result.ProviderTradeNo == "" || result.OccurredAt.IsZero() {
			return attempt, ErrConflict
		}
		fact := PaymentFact{OrderID: orderID, ProviderEventID: attempt.ChannelID + ":" + result.EventID,
			ProviderTradeNo: result.ProviderTradeNo, Status: "paid", AmountMinor: attempt.AmountMinor,
			Currency: attempt.Currency, OccurredAt: result.OccurredAt}
		if err := s.Facts.Record(ctx, accountID, fact); err != nil {
			return attempt, err
		}
		attempt.State = "paid"
		paid, err := s.Attempts.Put(ctx, attempt)
		if errors.Is(err, ErrConflict) {
			// The original page and the returned checkout window may confirm at
			// the same time. ZBoard records the identical provider event once;
			// acknowledge a concurrent local paid marker as the same result.
			current, getErr := s.Attempts.Get(ctx, accountID, orderID)
			if getErr == nil && current.State == "paid" && current.ProviderOrderID == attempt.ProviderOrderID && current.ChannelID == attempt.ChannelID {
				return current, nil
			}
		}
		return paid, err
	default:
		return attempt, ErrConflict
	}
}

func idempotencyKey(a Attempt) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", a.AccountID, a.OrderTradeNo, a.ChannelID, a.AttemptNumber)))
	return "zboard-" + hex.EncodeToString(h[:])
}

func validHTTPSURL(raw string) bool {
	if len(raw) > 2048 || strings.Contains(raw, "\\") || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Opaque == ""
}
