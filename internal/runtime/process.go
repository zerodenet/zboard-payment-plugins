package runtime

import (
	"context"
	"errors"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

// Open initializes the resources available to the installed native process.
// Commerce uses the process-authenticated, order-scoped public SDK.
func Open() (*Server, error) {
	store, err := openStorage()
	if err != nil {
		return nil, err
	}
	s := New()
	s.attempts = store
	host, err := pluginv1.HostClientFromEnvironment()
	if err != nil {
		store.Close()
		return nil, err
	}
	s.host = host
	adapter := nativeCommerce{host: host}
	if err := s.BindCommerce(CommercePorts{Orders: adapter, Facts: adapter, OrderReturnURL: trustedReturnURL}); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Server) Close() {
	if s.attempts != nil {
		s.attempts.Close()
	}
	if s.host != nil {
		s.host.Close()
	}
}

// CommercePorts contains only business services supplied by an authorized
// host adapter. Payment attempts remain owned and persisted by this plugin.
type CommercePorts struct {
	Orders         payment.OrderSource
	Facts          payment.FactRecorder
	OrderReturnURL func(context.Context, string, uint64) (string, error)
}

func (s *Server) BindCommerce(ports CommercePorts) error {
	if s.attempts == nil {
		return errors.New("plugin persistence has not been initialized")
	}
	_, err := s.BindCheckout(CheckoutPorts{Orders: ports.Orders, Facts: ports.Facts,
		Attempts: s.attempts, OrderReturnURL: ports.OrderReturnURL})
	return err
}

type Readiness struct {
	StorageReady  bool     `json:"storage_ready"`
	CheckoutReady bool     `json:"checkout_ready"`
	Missing       []string `json:"missing"`
}

func (s *Server) readiness() Readiness {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ready := Readiness{StorageReady: s.attempts != nil || s.checkout != nil, CheckoutReady: s.checkout != nil, Missing: []string{}}
	if !ready.StorageReady {
		ready.Missing = append(ready.Missing, "plugin_storage")
	}
	if !ready.CheckoutReady {
		ready.Missing = append(ready.Missing, "host_order_access", "host_payment_record", "trusted_order_return_url")
	}
	return ready
}
