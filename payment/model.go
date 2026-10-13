package payment

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var (
	ErrProviderMetadataUnavailable = errors.New("provider invoice currency metadata is unavailable")
	ErrNotFound                    = errors.New("payment attempt not found")
	ErrConflict                    = errors.New("payment attempt conflicts with order or channel")
	ErrPending                     = errors.New("payment creation is pending; retry with the same idempotency key")
	ErrCheckoutFailed              = errors.New("provider checkout has definitively failed")
	// ErrAmountPrecision guarantees rejection before any provider order is created.
	ErrAmountPrecision = errors.New("payment amount cannot be represented exactly")
)

// Order is an immutable ZBoard checkout snapshot. The core still owns its
// state transition, subscription, entitlement and fulfillment.
type Order struct {
	ID          uint64
	TradeNo     string
	Description string
	AccountID   string
	AmountMinor int64
	Currency    string
	Status      string
}

type CheckoutRequest struct {
	Reference      string
	AmountMinor    int64
	Currency       string
	Description    string
	ReturnURL      string
	IdempotencyKey string
}

type Checkout struct {
	ProviderOrderID string
	RedirectURL     string
	ExpiresAt       time.Time
	Status          string // pending, review, or paid on an idempotent retry
}

// PaymentResult is obtained through an authenticated server-to-server lookup.
// Browser redirects and unverified webhook bodies are never payment evidence.
type PaymentResult struct {
	ProviderOrderID string
	Reference       string
	ProviderTradeNo string
	EventID         string
	Status          string // pending, paid, failed, unknown, or reversed
	AmountMinor     int64
	Currency        string
	OccurredAt      time.Time
}

type PaymentFact struct {
	OrderID         uint64    `json:"order_id"`
	ProviderEventID string    `json:"provider_event_id"`
	ProviderTradeNo string    `json:"provider_trade_no"`
	Status          string    `json:"status"`
	AmountMinor     int64     `json:"amount_minor"`
	Currency        string    `json:"currency"`
	OccurredAt      time.Time `json:"occurred_at"`
}

type Attempt struct {
	AccountID       string `json:"account_id"`
	OrderID         uint64 `json:"order_id"`
	OrderTradeNo    string `json:"order_trade_no"`
	ChannelID       string `json:"channel_id"`
	AmountMinor     int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
	ProviderOrderID string `json:"provider_order_id,omitempty"`
	// ChannelSnapshot is private provider configuration stored in the host's
	// encrypted plugin namespace. Never serialize Attempt to a browser response.
	ChannelSnapshot []byte    `json:"channel_snapshot,omitempty"`
	ReturnURL       string    `json:"return_url,omitempty"`
	RedirectURL     string    `json:"redirect_url,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	State           string    `json:"state"` // creating, pending, paid, failed, review
	AttemptNumber   uint64    `json:"attempt_number"`
	PreviousOrders  []string  `json:"previous_orders,omitempty"`
	Revision        uint64    `json:"revision"`
}

type Channel interface {
	ID() string
	Describe(context.Context) (ChannelInfo, error)
	CreateCheckout(context.Context, CheckoutRequest) (Checkout, error)
	Lookup(context.Context, string) (PaymentResult, error)
}

// ConnectionProbe verifies merchant access without creating an order.
type ConnectionProbe interface {
	Probe(context.Context) error
}

// SnapshotChannel optionally freezes provider-owned metadata before an
// idempotent create is persisted. Settings remain private to the plugin.
type SnapshotChannel interface {
	PrepareSnapshot(context.Context, []byte, string) (Channel, []byte, error)
}

// NotificationHint identifies a provider order to reconcile. Even a signed
// notification is not payment proof; the coordinator must still call Lookup.
type NotificationHint struct {
	ProviderOrderID string
	Reference       string
	AmountMinor     int64
	Currency        string
	Status          string
}

// NotificationChannel is optional. Each provider authenticates its own
// notification before returning a hint for shared server-side reconciliation.
type NotificationChannel interface {
	InspectNotification([]byte, http.Header, time.Time) (NotificationHint, error)
}

type ChannelInfo struct {
	Currency      string
	Currencies    []string // supported invoice currencies for a hosted cashier
	LoginRequired bool
	TestMode      bool
}

type OrderSource interface {
	GetOrder(context.Context, string, uint64) (Order, error)
}

type FactRecorder interface {
	Record(context.Context, string, PaymentFact) error
}

// AttemptStore must atomically compare revisions. Create requires revision 0;
// Update requires the revision returned by Get or Create.
type AttemptStore interface {
	Get(context.Context, string, uint64) (Attempt, error)
	Put(context.Context, Attempt) (Attempt, error)
}

// NotificationAttemptLocator is an optional reverse lookup kept by the
// plugin. Public callbacks provide a provider order ID, never an account ID.
type NotificationAttemptLocator interface {
	FindByProviderOrder(context.Context, string, string) (Attempt, error)
}
