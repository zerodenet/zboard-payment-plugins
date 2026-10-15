package runtime

import (
	"context"
	"errors"
	"strconv"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func (s *Server) historicalMethods(ctx context.Context, actor, id, currency string, methods []Method) ([]Method, error) {
	orderID, err := strconv.ParseUint(id, 10, 64)
	if err != nil || orderID == 0 || strconv.FormatUint(orderID, 10) != id {
		return nil, payment.ErrConflict
	}
	s.mu.RLock()
	checkout := s.checkout
	s.mu.RUnlock()
	if checkout == nil || s.attempts == nil {
		return nil, payment.ErrConflict
	}
	order, err := checkout.ports.Orders.GetOrder(ctx, actor, orderID)
	if err != nil {
		return nil, err
	}
	if order.Currency != currency {
		return nil, payment.ErrConflict
	}
	attempt, err := s.attempts.historicalAttempt(ctx, orderID)
	if errors.Is(err, payment.ErrNotFound) {
		return methods, nil
	}
	if err != nil {
		return nil, err
	}
	var expiresAt = &attempt.ExpiresAt
	if attempt.State != "pending" || attempt.ExpiresAt.IsZero() {
		expiresAt = nil
	}
	for index, method := range methods {
		if method.ID == attempt.ChannelID {
			methods[index].CheckoutExpiresAt = expiresAt
			return methods, nil
		}
	}
	name := "原订单支付方式"
	for _, channel := range s.current().Channels {
		if channel.ID == attempt.ChannelID {
			name = channel.Name
		}
	}
	if len(methods) >= 16 {
		methods = methods[:15]
	}
	return append(methods, Method{ID: attempt.ChannelID, Name: name, Currency: currency, Disabled: true, CheckoutExpiresAt: expiresAt}), nil
}
