package tdcloud

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

var ErrInvalidNotification = errors.New("invalid TDCloud payment notification")

var _ payment.NotificationChannel = (*Client)(nil)

// Notification is a signed status snapshot. The plugin must still use Lookup
// before recording a payment fact; the snapshot only identifies what to check.
type Notification struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	APIVersion int       `json:"api_version"`
	CreatedAt  time.Time `json:"created_at"`
	Data       struct {
		OrderNo           string    `json:"order_no"`
		ExternalReference string    `json:"external_reference"`
		PaymentNo         string    `json:"payment_no"`
		Status            string    `json:"status"`
		StatusVersion     uint64    `json:"status_version"`
		Amount            string    `json:"amount"`
		AmountMinor       int64     `json:"amount_minor"`
		Currency          string    `json:"currency"`
		CurrencyDecimals  *int      `json:"currency_decimals"`
		EventID           string    `json:"event_id"`
		ProviderTradeNo   string    `json:"provider_trade_no"`
		OccurredAt        time.Time `json:"occurred_at"`
	} `json:"data"`
}

// VerifyNotification follows TDCloud's timestamp + "." + raw-body HMAC
// contract. The event header must agree with the signed envelope.
func VerifyNotification(body []byte, eventID, timestamp, signature, secret string, now time.Time) (Notification, error) {
	var event Notification
	if len(body) == 0 || len(body) > 64<<10 || len(secret) < 32 || len(secret) > 256 {
		return event, ErrInvalidNotification
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || !strings.HasPrefix(signature, "sha256=") {
		return event, ErrInvalidNotification
	}
	at := time.Unix(seconds, 0)
	if now.Sub(at) > 5*time.Minute || at.Sub(now) > 5*time.Minute {
		return event, ErrInvalidNotification
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil || len(provided) != sha256.Size {
		return event, ErrInvalidNotification
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), provided) {
		return event, ErrInvalidNotification
	}
	event, err = parseNotification(body)
	if err != nil || event.ID != eventID {
		return Notification{}, ErrInvalidNotification
	}
	return event, nil
}

// parseNotification accepts an untrusted delivery hint. Its fields must never
// settle an order without a matching stored attempt and authenticated lookup.
func parseNotification(body []byte) (Notification, error) {
	var event Notification
	if len(body) == 0 || len(body) > 64<<10 {
		return event, ErrInvalidNotification
	}
	if err := json.Unmarshal(body, &event); err != nil || event.ID == "" ||
		event.Type != "merchant_payment.status_changed" || event.APIVersion != 1 || event.CreatedAt.IsZero() ||
		event.Data.OrderNo == "" || event.Data.ExternalReference == "" ||
		event.Data.StatusVersion == 0 || !validCurrency(event.Data.Currency) {
		return Notification{}, ErrInvalidNotification
	}
	if event.Data.Amount != "" {
		if _, err := parseHostAmount(event.Data.Amount); err != nil {
			return Notification{}, ErrInvalidNotification
		}
	} else if event.Data.AmountMinor <= 0 || event.Data.AmountMinor > maxProviderMinor || event.Data.CurrencyDecimals == nil || !validDecimals(*event.Data.CurrencyDecimals) {
		return Notification{}, ErrInvalidNotification // historical signed snapshots
	}
	switch event.Data.Status {
	case "pending", "review", "paid", "expired", "refunded", "chargeback":
	default:
		return Notification{}, ErrInvalidNotification
	}
	return event, nil
}

// ParseNotification reads the untrusted body for provider-order routing.
// The parsed fields cannot prove payment; always reconcile through Lookup.
func ParseNotification(body []byte) (Notification, error) { return parseNotification(body) }

// InspectNotification requires TDCloud's signed delivery headers. The
// coordinator still checks the stored attempt and authenticated order lookup.
func (c *Client) InspectNotification(body []byte, headers http.Header, now time.Time) (payment.NotificationHint, error) {
	if c.notifyEnabled == nil || !*c.notifyEnabled || c.notifySecret == "" {
		return payment.NotificationHint{}, ErrInvalidNotification
	}
	eventID, timestamp, signature := headers.Get("X-TDCloud-Event-ID"), headers.Get("X-TDCloud-Timestamp"), headers.Get("X-TDCloud-Signature")
	event, err := VerifyNotification(body, eventID, timestamp, signature, c.notifySecret, now)
	if err != nil || (c.legacyPaymentNo != "" && event.Data.PaymentNo != c.legacyPaymentNo) {
		return payment.NotificationHint{}, ErrInvalidNotification
	}
	amount, err := c.resultAmount(event.Data.Amount, event.Data.AmountMinor, event.Data.Currency, event.Data.CurrencyDecimals)
	if err != nil {
		return payment.NotificationHint{}, ErrInvalidNotification
	}
	return payment.NotificationHint{ProviderOrderID: event.Data.OrderNo, Reference: event.Data.ExternalReference,
		AmountMinor: amount, Currency: event.Data.Currency, Status: event.Data.Status, StatusVersion: event.Data.StatusVersion}, nil
}
