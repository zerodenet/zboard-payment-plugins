package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) hostedAction(ctx context.Context, r *pluginv1.PageActionRequest) (*pluginv1.PageActionResponse, error) {
	if r.Purpose != "slot" || r.PageId != "" ||
		!((r.Slot == pluginv1.CheckoutPaymentSlot && r.SlotId == "payment-methods") ||
			(r.Slot == pluginv1.OrderPaymentSlot && r.SlotId == "order-payment")) {
		return nil, status.Error(codes.PermissionDenied, "支付入口未授权")
	}
	var input struct {
		OptionID string `json:"option_id"`
	}
	if !decodeAction(r.PayloadJson, &input) || !channelIDPattern.MatchString(input.OptionID) {
		return nil, status.Error(codes.InvalidArgument, "支付方式无效")
	}
	order, err := r.Checkout()
	if err != nil || order.OrderID == 0 || order.AccountID == 0 || order.TradeNo == "" || order.PayableAmount <= 0 ||
		order.CurrencyPrecision != 2 || !currencyPattern.MatchString(order.Currency) {
		return nil, status.Error(codes.InvalidArgument, "缺少可信订单上下文")
	}
	s.mu.RLock()
	checkout := s.checkout
	s.mu.RUnlock()
	if checkout == nil {
		return nil, status.Error(codes.Unavailable, "支付接线不可用")
	}
	ctx = context.WithValue(ctx, trustedCheckoutKey{}, trustedCheckout{actor: r.ActorId, order: order})
	owned, err := checkout.ports.Orders.GetOrder(ctx, r.ActorId, uint64(order.OrderID))
	if err != nil {
		return nil, hostedError(err)
	}
	if owned.Status == "paid" {
		return hostedResponse(map[string]any{"state": "paid", "checkout_url": order.ReturnURL})
	}
	if owned.Status != "pending" {
		return nil, status.Error(codes.FailedPrecondition, "订单不可支付")
	}
	if s.attempts != nil {
		if err := s.attempts.rememberAuthority(ctx, uint64(order.OrderID), r.ActorId); err != nil {
			return nil, hostedError(err)
		}
	}
	var view CheckoutView
	if r.Action == "payments.prepare" {
		view, err = checkout.Start(ctx, r.ActorId, uint64(order.OrderID), input.OptionID)
	} else {
		attempt, readErr := checkout.ports.Attempts.Get(ctx, r.ActorId, uint64(order.OrderID))
		if readErr != nil {
			return nil, hostedError(readErr)
		}
		if attempt.ChannelID != input.OptionID {
			return nil, status.Error(codes.FailedPrecondition, "请核对该订单原支付方式")
		}
		view, err = checkout.Confirm(ctx, r.ActorId, uint64(order.OrderID))
		if errors.Is(err, payment.ErrPending) {
			err = nil
		}
	}
	if err != nil {
		return nil, hostedError(err)
	}
	if r.Action == "payments.prepare" && view.RedirectURL == "" {
		// The provider may return an already-paid/review order on an ambiguous
		// create retry. Reconcile and navigate back to the host receipt page.
		if _, err := checkout.Confirm(ctx, r.ActorId, uint64(order.OrderID)); err != nil && !errors.Is(err, payment.ErrPending) {
			return nil, hostedError(err)
		}
		view.RedirectURL = order.ReturnURL
	}
	if r.Action == "payments.prepare" {
		u, err := url.Parse(view.RedirectURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.ContainsAny(view.RedirectURL, "\r\n\\") {
			return nil, status.Error(codes.FailedPrecondition, "收银台地址无效")
		}
		return hostedResponse(map[string]any{"checkout_url": view.RedirectURL})
	}
	return hostedResponse(map[string]any{"state": view.State})
}

func hostedResponse(data any) (*pluginv1.PageActionResponse, error) {
	raw, err := json.Marshal(data)
	return &pluginv1.PageActionResponse{ResultJson: raw}, err
}

func hostedError(err error) error {
	code := actionErrorCode(err)
	switch code {
	case "amount_unrepresentable":
		return status.Error(codes.InvalidArgument, "订单金额不能按所选渠道精度收款，请选择其他方式")
	case "permission_denied":
		return status.Error(codes.PermissionDenied, "支付授权不可用，请重新进入订单页")
	case "invalid_argument":
		return status.Error(codes.InvalidArgument, "订单支付请求无效")
	case "conflict", "checkout_failed":
		return status.Error(codes.FailedPrecondition, "支付状态冲突，请核对订单")
	case "not_found":
		return status.Error(codes.NotFound, "该订单尚无可核对的支付尝试")
	default:
		return status.Error(codes.Unavailable, "支付暂时不可用，请使用同一订单重试")
	}
}
