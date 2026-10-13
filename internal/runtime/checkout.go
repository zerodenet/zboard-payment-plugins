package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

// CheckoutPorts are trusted, account-scoped host services. The caller must
// obtain accountID from the host session, never from a browser payload.
type CheckoutPorts struct {
	Orders         payment.OrderSource
	Facts          payment.FactRecorder
	Attempts       payment.AttemptStore
	OrderReturnURL func(context.Context, string, uint64) (string, error)
}

// CheckoutCoordinator joins configured providers to the shared payment state
// machine. It has no browser or HTTP entrypoint; a payment slot can call it
// after the host has supplied authenticated commerce ports.
type CheckoutCoordinator struct {
	server *Server
	ports  CheckoutPorts
}

func (s *Server) Checkout(ports CheckoutPorts) (*CheckoutCoordinator, error) {
	if ports.Orders == nil || ports.Facts == nil || ports.Attempts == nil || ports.OrderReturnURL == nil {
		return nil, errors.New("trusted checkout ports are required")
	}
	return &CheckoutCoordinator{server: s, ports: ports}, nil
}

type CheckoutView struct {
	OrderID     uint64 `json:"order_id"`
	State       string `json:"state"`
	RedirectURL string `json:"redirect_url,omitempty"`
}

type providerSnapshot struct {
	Type     string          `json:"type"`
	Settings json.RawMessage `json:"settings"`
}

func snapshotChannel(item channelConfig) ([]byte, error) {
	return json.Marshal(providerSnapshot{Type: item.Type, Settings: item.Settings})
}

func (c *CheckoutCoordinator) channelFromSnapshot(attempt payment.Attempt) (payment.Channel, error) {
	var snapshot providerSnapshot
	if err := json.Unmarshal(attempt.ChannelSnapshot, &snapshot); err != nil {
		return nil, err
	}
	return c.server.cachedChannel(channelConfig{ID: attempt.ChannelID, Type: snapshot.Type, Settings: snapshot.Settings}, true)
}

func (c *CheckoutCoordinator) resolveChannel(attempt payment.Attempt) (payment.Channel, error) {
	return c.channelFromSnapshot(attempt)
}

func (c *CheckoutCoordinator) Methods(ctx context.Context, currency string) ([]Method, error) {
	return c.server.ListMethods(ctx, currency)
}

func (c *CheckoutCoordinator) Start(ctx context.Context, accountID string, orderID uint64, channelID string) (CheckoutView, error) {
	saved, lookupErr := c.ports.Attempts.Get(ctx, accountID, orderID)
	if lookupErr != nil && !errors.Is(lookupErr, payment.ErrNotFound) {
		return CheckoutView{}, lookupErr
	}
	if lookupErr == nil && saved.ChannelID != channelID && saved.State != "failed" {
		return CheckoutView{}, payment.ErrConflict
	}
	var returnURL string
	if lookupErr == nil && saved.ChannelID == channelID && (saved.State == "creating" || saved.State == "pending") {
		returnURL = saved.ReturnURL
	} else {
		var err error
		returnURL, err = c.ports.OrderReturnURL(ctx, accountID, orderID)
		if err != nil {
			return CheckoutView{}, err
		}
	}
	var channel payment.Channel
	var snapshot []byte
	var prepare func(context.Context, payment.Channel, payment.Order) (payment.Channel, []byte, error)
	if lookupErr == nil && saved.ChannelID == channelID && (saved.State == "creating" || saved.State == "pending") && len(saved.ChannelSnapshot) > 0 {
		channel, lookupErr = c.resolveChannel(saved)
		snapshot = saved.ChannelSnapshot
		if lookupErr != nil {
			return CheckoutView{}, fmt.Errorf("restore payment channel %s: %w", channelID, lookupErr)
		}
	} else {
		var selected *channelConfig
		for _, item := range c.server.current().Channels {
			if item.ID == channelID && (item.Enabled || (lookupErr == nil && saved.ChannelID == channelID && saved.State != "failed")) {
				copy := item
				selected = &copy
				break
			}
		}
		if selected == nil {
			return CheckoutView{}, payment.ErrConflict
		}
		var err error
		channel, err = c.server.channelFor(*selected)
		if err != nil {
			return CheckoutView{}, fmt.Errorf("payment channel %s: %w", channelID, err)
		}
		prepare = func(ctx context.Context, active payment.Channel, order payment.Order) (payment.Channel, []byte, error) {
			return c.server.prepareSnapshot(ctx, *selected, active, order.Currency)
		}
	}
	service := payment.Service{Orders: c.ports.Orders, Facts: c.ports.Facts,
		Attempts: c.ports.Attempts, Channels: map[string]payment.Channel{channelID: channel},
		ChannelSnapshot: snapshot, ResolveChannel: c.resolveChannel, PrepareChannel: prepare}
	attempt, err := service.Start(ctx, accountID, orderID, channelID, returnURL)
	return checkoutView(attempt), err
}

func (c *CheckoutCoordinator) Confirm(ctx context.Context, accountID string, orderID uint64) (CheckoutView, error) {
	attempt, err := c.ports.Attempts.Get(ctx, accountID, orderID)
	if err != nil {
		return CheckoutView{}, err
	}
	channels := map[string]payment.Channel{}
	if len(attempt.ChannelSnapshot) == 0 && attempt.State != "paid" && attempt.State != "failed" && attempt.State != "review" {
		for _, item := range c.server.current().Channels {
			if item.ID != attempt.ChannelID {
				continue
			}
			channel, err := c.server.channelFor(item)
			if err != nil {
				return CheckoutView{}, fmt.Errorf("payment channel %s: %w", item.ID, err)
			}
			channels[item.ID] = channel
			break
		}
	}
	service := payment.Service{Orders: c.ports.Orders, Facts: c.ports.Facts,
		Attempts: c.ports.Attempts, Channels: channels, ResolveChannel: c.resolveChannel}
	attempt, err = service.Reconcile(ctx, accountID, orderID)
	return checkoutView(attempt), err
}

func checkoutView(attempt payment.Attempt) CheckoutView {
	return CheckoutView{OrderID: attempt.OrderID, State: attempt.State, RedirectURL: attempt.RedirectURL}
}
