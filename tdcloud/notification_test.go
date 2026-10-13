package tdcloud

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"
)

func signedNotification(body []byte, timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyNotificationRequiresRawBodySignatureAndFreshMatchingEvent(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	secret := "independent-merchant-notification-secret-123456"
	body, _ := json.Marshal(map[string]any{
		"id": "MN123", "type": "merchant_payment.status_changed", "api_version": 1, "created_at": now,
		"data": map[string]any{"order_no": "MP123", "external_reference": "zb-42", "payment_no": "stripe-usd",
			"status": "paid", "status_version": 2, "amount_minor": 9007199254740991, "currency": "USD", "currency_decimals": 2, "event_id": "PE123"},
	})
	timestamp := strconv.FormatInt(now.Unix(), 10)
	signature := signedNotification(body, timestamp, secret)
	event, err := VerifyNotification(body, "MN123", timestamp, signature, secret, now)
	if err != nil || event.Data.AmountMinor != 9007199254740991 || event.Data.StatusVersion != 2 {
		t.Fatalf("valid signed notification: %+v %v", event, err)
	}
	for name, input := range map[string]struct {
		body      []byte
		eventID   string
		timestamp string
		signature string
	}{
		"tampered body":   {append(append([]byte(nil), body...), ' '), "MN123", timestamp, signature},
		"wrong event":     {body, "MN999", timestamp, signature},
		"stale time":      {body, "MN123", strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10), signature},
		"wrong signature": {body, "MN123", timestamp, "sha256=" + hex.EncodeToString(make([]byte, 32))},
	} {
		if _, err := VerifyNotification(input.body, input.eventID, input.timestamp, input.signature, secret, now); !errors.Is(err, ErrInvalidNotification) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}
