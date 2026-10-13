package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

type actionError struct {
	Code string `json:"code"`
}

type actionResult struct {
	Data  any          `json:"data,omitempty"`
	Error *actionError `json:"error,omitempty"`
}

func actionResponse(data any, code string) (*pluginv1.PageActionResponse, error) {
	result := actionResult{Data: data}
	if code != "" {
		result = actionResult{Error: &actionError{Code: code}}
	}
	raw, err := json.Marshal(result)
	return &pluginv1.PageActionResponse{ResultJson: raw}, err
}

func decodeAction(raw []byte, out any) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || len(raw) > 64<<10 || trimmed[0] != '{' {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out) == nil && decoder.Decode(new(any)) == io.EOF
}

// HandlePageAction uses only the actor supplied by the authenticated host RPC.
// No browser payload may select an account, amount, fact, or return URL.
func (s *Server) HandlePageAction(ctx context.Context, r *pluginv1.PageActionRequest) (*pluginv1.PageActionResponse, error) {
	if r == nil || r.Surface != "account" || r.ActorId == "" ||
		(r.PageId != "" && r.PageId != "payment-methods" && r.PageId != "order-payment") {
		return actionResponse(nil, "permission_denied")
	}
	switch r.Action {
	case "payments.prepare", "payments.reconcile":
		return s.hostedAction(ctx, r)
	case "payments.status":
		if !decodeAction(r.PayloadJson, new(struct{})) {
			return actionResponse(nil, "invalid_argument")
		}
		return actionResponse(s.readiness(), "")
	case "payments.methods":
		var input struct {
			Currency string `json:"currency"`
			OrderID  string `json:"order_id,omitempty"`
		}
		if !decodeAction(r.PayloadJson, &input) || !currencyPattern.MatchString(input.Currency) {
			return actionResponse(nil, "invalid_argument")
		}
		methods, err := s.ListMethods(ctx, input.Currency)
		if methods == nil {
			methods = []Method{}
		}
		if input.OrderID != "" {
			historical, historyErr := s.historicalMethods(ctx, r.ActorId, input.OrderID, input.Currency, methods)
			if historyErr != nil {
				err = historyErr
			} else if len(historical) > 0 {
				methods, err = historical, nil
			}
		}
		return actionResponse(methods, actionErrorCode(err))
	case "payments.start", "payments.confirm", "payments.order":
		return s.orderAction(ctx, r)
	default:
		return actionResponse(nil, "invalid_argument")
	}
}

func (s *Server) orderAction(ctx context.Context, r *pluginv1.PageActionRequest) (*pluginv1.PageActionResponse, error) {
	var input struct {
		OrderID   string `json:"order_id"`
		ChannelID string `json:"channel_id,omitempty"`
	}
	if !decodeAction(r.PayloadJson, &input) {
		return actionResponse(nil, "invalid_argument")
	}
	id, err := strconv.ParseUint(input.OrderID, 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != input.OrderID ||
		(r.Action == "payments.start" && !channelIDPattern.MatchString(input.ChannelID)) ||
		(r.Action != "payments.start" && input.ChannelID != "") {
		return actionResponse(nil, "invalid_argument")
	}
	s.mu.RLock()
	checkout := s.checkout
	s.mu.RUnlock()
	if checkout == nil {
		return actionResponse(nil, "host_capability_missing")
	}
	if r.Action != "payments.start" {
		order, readErr := checkout.ports.Orders.GetOrder(ctx, r.ActorId, id)
		if readErr != nil {
			return actionResponse(nil, actionErrorCode(readErr))
		}
		if order.ID != id || order.AccountID != r.ActorId {
			return actionResponse(nil, "permission_denied")
		}
		if r.Action == "payments.order" {
			return actionResponse(struct {
				ID       string `json:"id"`
				Status   string `json:"status"`
				Currency string `json:"currency"`
			}{input.OrderID, order.Status, order.Currency}, "")
		}
	}
	var view CheckoutView
	if r.Action == "payments.start" {
		view, err = checkout.Start(ctx, r.ActorId, id, input.ChannelID)
	} else {
		view, err = checkout.Confirm(ctx, r.ActorId, id)
		if errors.Is(err, payment.ErrPending) {
			err = nil // a creating attempt is a normal confirmation result
		}
	}
	return actionResponse(struct {
		OrderID     string `json:"order_id"`
		State       string `json:"state"`
		RedirectURL string `json:"redirect_url,omitempty"`
	}{strconv.FormatUint(view.OrderID, 10), view.State, view.RedirectURL}, actionErrorCode(err))
}

func actionErrorCode(err error) string {
	var hostErr *pluginv1.HostCallError
	if errors.As(err, &hostErr) {
		switch hostErr.Code {
		case "forbidden":
			return "permission_denied"
		case "conflict":
			return "conflict"
		case "invalid_request":
			return "invalid_argument"
		case "capability_unavailable":
			return "host_capability_missing"
		case "rate_limited":
			return "rate_limited"
		case "deadline_exceeded":
			return "deadline_exceeded"
		}
	}
	switch {
	case err == nil:
		return ""
	case errors.Is(err, payment.ErrProviderMetadataUnavailable):
		return "provider_metadata_missing"
	case errors.Is(err, payment.ErrNotFound):
		return "not_found"
	case errors.Is(err, payment.ErrConflict):
		return "conflict"
	case errors.Is(err, payment.ErrCheckoutFailed):
		return "checkout_failed"
	case errors.Is(err, payment.ErrAmountPrecision):
		return "amount_unrepresentable"
	case errors.Is(err, payment.ErrPending):
		return "pending"
	default:
		return "temporarily_unavailable"
	}
}
