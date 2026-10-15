package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

type nativeCommerce struct{ host *pluginv1.HostClient }
type trustedCheckoutKey struct{}
type trustedCheckout struct {
	actor string
	order pluginv1.CheckoutContext
}

func (c nativeCommerce) GetOrder(ctx context.Context, principal string, id uint64) (payment.Order, error) {
	if id == 0 || uint64(uint(id)) != id || principal == "" {
		return payment.Order{}, payment.ErrConflict
	}
	var page pluginv1.CommerceOrderPage
	err := retryNative(ctx, func() error {
		return c.host.ListOrders(ctx, pluginv1.CommerceOrdersRequest{PrincipalID: principal, ID: uint(id), Limit: 1}, &page)
	})
	if err != nil {
		return payment.Order{}, err
	}
	if len(page.Items) != 1 || uint64(page.Items[0].ID) != id {
		return payment.Order{}, payment.ErrNotFound
	}
	order := page.Items[0]
	if authority, ok := ctx.Value(trustedCheckoutKey{}).(trustedCheckout); ok &&
		(authority.actor != principal || uint64(authority.order.OrderID) != id ||
			authority.order.AccountID != order.AccountID || authority.order.TradeNo != order.TradeNo ||
			authority.order.PayableAmount != order.PayableAmount || authority.order.Currency != order.Currency) {
		return payment.Order{}, payment.ErrConflict
	}
	// AccountID in the payment core is the private host authority. The SDK
	// validates the actual account/order scope; the browser never receives it.
	return payment.Order{ID: id, TradeNo: order.TradeNo, AccountID: principal,
		AmountMinor: order.PayableAmount, Currency: order.Currency, Status: order.Status}, nil
}

func (c nativeCommerce) Record(ctx context.Context, principal string, fact payment.PaymentFact) error {
	if fact.OrderID == 0 || uint64(uint(fact.OrderID)) != fact.OrderID {
		return payment.ErrConflict
	}
	return retryNative(ctx, func() error {
		result, err := c.host.RecordPayment(ctx, pluginv1.PaymentFactRequest{PrincipalID: principal, OrderID: uint(fact.OrderID),
			ProviderEventID: fact.ProviderEventID, ProviderTradeNo: fact.ProviderTradeNo, Status: fact.Status,
			AmountMinor: fact.AmountMinor, Currency: fact.Currency, OccurredAt: fact.OccurredAt})
		var hostErr *pluginv1.HostCallError
		if errors.As(err, &hostErr) && hostErr.Code == "conflict" {
			return payment.ErrFactNeedsReview
		}
		if err == nil && (uint64(result.OrderID) != fact.OrderID || result.Status != "paid") {
			return payment.ErrConflict
		}
		return err
	})
}

func trustedReturnURL(ctx context.Context, principal string, id uint64) (string, error) {
	authority, ok := ctx.Value(trustedCheckoutKey{}).(trustedCheckout)
	if !ok || authority.actor != principal || uint64(authority.order.OrderID) != id || authority.order.ReturnURL == "" {
		return "", payment.ErrConflict
	}
	return authority.order.ReturnURL, nil
}

func retryNative(ctx context.Context, call func() error) error {
	for attempt := 0; ; attempt++ {
		err := call()
		var hostErr *pluginv1.HostCallError
		temporary := errors.As(err, &hostErr) && (hostErr.Code == "temporary_unavailable" || hostErr.Code == "deadline_exceeded")
		if !temporary || attempt == 5 {
			return err
		}
		timer := time.NewTimer(time.Duration(1<<attempt) * 50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
