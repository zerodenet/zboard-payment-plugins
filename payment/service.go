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
	attempt = CloneAttempt(attempt)
	for _, historical := range attempt.History {
		if historical.VerifiedFact == nil || historical.FactState == "recorded" {
			continue
		}
		if historical.FactState == "review" && attempt.SettledProvider == "" {
			return attempt, ErrFactNeedsReview
		}
		if historical.FactState == "pending" {
			attempt, err = s.deliverFact(ctx, accountID, orderID, historical.ChannelID, historical.ProviderOrderID)
			if err != nil {
				return attempt, err
			}
		}
	}
	if attempt.VerifiedFact != nil && attempt.FactState == "pending" {
		attempt, err = s.deliverFact(ctx, accountID, orderID, attempt.ChannelID, attempt.ProviderOrderID)
		if err != nil {
			return attempt, err
		}
	}
	if returnURL != "" && returnURL != attempt.ReturnURL && attempt.State != "failed" {
		return Attempt{}, ErrConflict
	}
	if attempt.State == "paid" || attempt.SettledProvider != "" {
		if attempt.FactState == "review" && attempt.SettledProvider == "" {
			return attempt, ErrFactNeedsReview
		}
		attempt.RedirectURL, attempt.State = "", "paid"
		return attempt, nil
	}
	if attempt.State == "failed" && attempt.ProviderOrderID != "" {
		attempt, err = s.Reconcile(ctx, accountID, orderID)
		if err != nil {
			return attempt, err
		}
		if attempt.State == "paid" || attempt.SettledProvider != "" {
			attempt.RedirectURL, attempt.State = "", "paid"
			return attempt, nil
		}
	}
	// A saved redirect is usable only within its provider-issued deadline.
	// Expiry alone cannot release an order: a late payment may be in flight.
	if attempt.State == "pending" && attempt.ProviderOrderID != "" &&
		(attempt.RedirectURL == "" || !attempt.ExpiresAt.After(time.Now())) {
		attempt, err = s.Reconcile(ctx, accountID, orderID)
		if err != nil {
			return attempt, err
		}
		switch attempt.State {
		case "paid":
			attempt.RedirectURL = "" // return to the host receipt, never the old cashier
			return attempt, nil
		case "failed":
			// The authenticated lookup confirmed terminal failure. Reuse the
			// business order below with a new, persisted attempt/key.
		default:
			return attempt, ErrCheckoutExpiryPending
		}
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
		if attempt.ProviderOrderID != "" {
			attempt.History = append(attempt.History, archived(attempt))
			attempt.PreviousOrders = append(attempt.PreviousOrders, attempt.ProviderOrderID)
		}
		attempt.ChannelID, attempt.ProviderOrderID, attempt.RedirectURL = channelID, "", ""
		attempt.ChannelSnapshot = append([]byte(nil), s.ChannelSnapshot...)
		attempt.ExpiresAt, attempt.State, attempt.AttemptNumber = time.Time{}, "creating", attempt.AttemptNumber+1
		attempt.ProviderVersion, attempt.VerifiedFact, attempt.FactState, attempt.ReviewReason, attempt.NextCheckAt = 0, nil, "", "", time.Time{}
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
	if checkout.ProviderOrderID == "" || (checkout.Status != "" && checkout.Status != "pending" && checkout.Status != "review" && checkout.Status != "paid" && checkout.Status != "failed") ||
		((checkout.Status == "" || checkout.Status == "pending") && (!validHTTPSURL(checkout.RedirectURL) || !checkout.ExpiresAt.After(time.Now()))) {
		return Attempt{}, errors.New("channel returned an invalid checkout")
	}
	attempt.ProviderOrderID, attempt.State = checkout.ProviderOrderID, "pending"
	attempt.ProviderVersion = checkout.StatusVersion
	if checkout.Status == "failed" {
		attempt.State = "failed"
	}
	attempt.ExpiresAt = checkout.ExpiresAt
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
	if err == nil && checkout.Status == "failed" {
		return attempt, ErrCheckoutFailed
	}
	return attempt, err
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
