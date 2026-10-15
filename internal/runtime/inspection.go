package runtime

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

var providerStatePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Inspection reads an owned attempt without renewing, settling or changing it.
// No credential, channel configuration or checkout bearer URL is returned.
type paymentInspection struct {
	OrderID         uint64    `json:"order_id"`
	ProviderOrderID string    `json:"provider_order_id"`
	ProviderState   string    `json:"provider_state"`
	State           string    `json:"state"`
	ExpiresAt       time.Time `json:"expires_at"`
	CheckedAt       time.Time `json:"checked_at"`
}

func (s *Server) inspectAction(ctx context.Context, r *pluginv1.PageActionRequest) (*pluginv1.PageActionResponse, error) {
	if r.Purpose != "slot" || r.PageId != "" ||
		!((r.Slot == pluginv1.CheckoutPaymentSlot && r.SlotId == "payment-methods") ||
			(r.Slot == pluginv1.OrderPaymentSlot && r.SlotId == "order-payment")) {
		return actionResponse(nil, "permission_denied")
	}
	var input struct {
		OrderID string `json:"order_id"`
	}
	if !decodeAction(r.PayloadJson, &input) {
		return actionResponse(nil, "invalid_argument")
	}
	id, err := strconv.ParseUint(input.OrderID, 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != input.OrderID {
		return actionResponse(nil, "invalid_argument")
	}
	s.mu.RLock()
	checkout := s.checkout
	s.mu.RUnlock()
	if checkout == nil || s.attempts == nil {
		return actionResponse(nil, "host_capability_missing")
	}
	// Authorization precedes access to the historical private authority/index.
	order, err := checkout.ports.Orders.GetOrder(ctx, r.ActorId, id)
	if err != nil {
		return actionResponse(nil, actionErrorCode(err))
	}
	attempt, err := s.attempts.historicalAttempt(ctx, id)
	if err != nil {
		return actionResponse(nil, actionErrorCode(err))
	}
	if order.ID != id || order.AccountID != r.ActorId || attempt.OrderTradeNo != order.TradeNo || attempt.AmountMinor != order.AmountMinor ||
		!strings.EqualFold(attempt.Currency, order.Currency) {
		return actionResponse(nil, "conflict")
	}
	if attempt.ProviderOrderID == "" {
		return actionResponse(nil, "pending")
	}
	channel, err := checkout.resolveChannel(attempt)
	if err != nil || channel == nil || channel.ID() != attempt.ChannelID {
		return actionResponse(nil, "provider_configuration_invalid")
	}
	result, err := channel.Lookup(ctx, attempt.ProviderOrderID)
	if err != nil {
		code := "provider_lookup_failed"
		if diagnostic, ok := channel.(payment.LookupDiagnostic); ok {
			switch diagnostic.LookupProblem(err) {
			case "provider_oauth_failed":
				code = "provider_oauth_failed"
			case "provider_access_denied":
				code = "provider_access_denied"
			case "provider_order_not_found":
				code = "provider_order_not_found"
			}
		}
		return actionResponse(nil, code)
	}
	if result.ProviderOrderID != attempt.ProviderOrderID || result.Reference != attempt.OrderTradeNo ||
		result.AmountMinor != attempt.AmountMinor || !strings.EqualFold(result.Currency, attempt.Currency) {
		return actionResponse(nil, "provider_order_mismatch")
	}
	providerState := result.ProviderState
	if providerState == "" {
		providerState = result.Status
	}
	if !providerStatePattern.MatchString(providerState) {
		return actionResponse(nil, "provider_lookup_failed")
	}
	return actionResponse(paymentInspection{OrderID: id, ProviderOrderID: attempt.ProviderOrderID,
		ProviderState: providerState, State: result.Status, ExpiresAt: attempt.ExpiresAt, CheckedAt: time.Now().UTC()}, "")
}
