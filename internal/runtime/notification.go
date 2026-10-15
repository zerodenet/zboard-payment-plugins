package runtime

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

// BindCheckout makes the same trusted checkout ports available to the public
// notification route. A caller binds them once when host commerce ports exist.
func (s *Server) BindCheckout(ports CheckoutPorts) (*CheckoutCoordinator, error) {
	coordinator, err := s.Checkout(ports)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.checkout = coordinator
	s.mu.Unlock()
	return coordinator, nil
}

// ConfirmNotification treats a provider callback as a request to reconcile.
// The caller locates the account and order through the plugin-owned provider
// index; callback contents alone never select an arbitrary business order.
func (c *CheckoutCoordinator) ConfirmNotification(ctx context.Context, accountID string, orderID uint64, body []byte, headers http.Header, now time.Time) (CheckoutView, error) {
	attempt, err := c.ports.Attempts.Get(ctx, accountID, orderID)
	if err != nil {
		return CheckoutView{}, err
	}
	return c.confirmAttemptNotification(ctx, attempt, body, headers, now)
}

func (c *CheckoutCoordinator) confirmAttemptNotification(ctx context.Context, attempt payment.Attempt, body []byte, headers http.Header, now time.Time) (CheckoutView, error) {
	if attempt.ProviderOrderID == "" || len(attempt.ChannelSnapshot) == 0 {
		return CheckoutView{}, payment.ErrPending
	}
	channel, err := c.channelFromSnapshot(attempt)
	if err != nil {
		return CheckoutView{}, err
	}
	provider, ok := channel.(payment.NotificationChannel)
	if !ok {
		return CheckoutView{}, payment.ErrConflict
	}
	hint, err := provider.InspectNotification(body, headers, now)
	if err != nil {
		return CheckoutView{}, err
	}
	if hint.ProviderOrderID != attempt.ProviderOrderID || hint.Reference != attempt.OrderTradeNo ||
		hint.AmountMinor != attempt.AmountMinor || !strings.EqualFold(hint.Currency, attempt.Currency) {
		return CheckoutView{}, payment.ErrConflict
	}
	service := payment.Service{Orders: c.ports.Orders, Facts: c.ports.Facts, Attempts: c.ports.Attempts, ResolveChannel: c.resolveChannel}
	force := !(attempt.State == "paid" && attempt.FactState == "recorded" && (hint.Status == "paid" || hint.StatusVersion <= attempt.ProviderVersion))
	root, err := service.ReconcileProvider(ctx, attempt.AccountID, attempt.OrderID, attempt.ChannelID, hint.ProviderOrderID, hint.StatusVersion, force)
	view := checkoutView(root)
	if err != nil {
		return view, err
	}
	checked, found := payment.FindAttempt(root, attempt.ChannelID, hint.ProviderOrderID)
	if hint.Status == "paid" && (!found || checked.VerifiedFact == nil) {
		return view, payment.ErrPending
	}
	return view, nil
}
