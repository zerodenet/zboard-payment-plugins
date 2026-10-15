package tdcloud

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func validateExpiry(seconds *int64) error {
	if seconds != nil && (*seconds < 60 || *seconds > 7200) {
		return errors.New("TDCloud expiry must be 60-7200 seconds")
	}
	return nil
}

func validReturnURL(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 2048 || strings.Contains(value, "\\") || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}

// Only optional presentation fields are trimmed. Order identity, amount,
// currency and idempotency key are never rewritten. Host-owned return URLs are
// already validated and pinned by the attempt; direct adapter callers get the
// same canonical return comparison TDCloud applies before creation.
func (c *Client) createBody(request payment.CheckoutRequest, amount string) (map[string]any, string, error) {
	returnURL, description := strings.TrimSpace(request.ReturnURL), strings.TrimSpace(request.Description)
	if !validReturnURL(returnURL) || len(description) > 500 {
		return nil, "", payment.ErrConflict
	}
	if err := validateExpiry(c.expiresIn); err != nil {
		return nil, "", err
	}
	body := map[string]any{"external_reference": request.Reference, "idempotency_key": request.IdempotencyKey, "amount": amount, "currency": request.Currency}
	if description != "" {
		body["description"] = description
	}
	if returnURL != "" {
		body["return_url"] = returnURL
	}
	if c.expiresIn != nil {
		body["expires_in"] = *c.expiresIn
	}
	if c.usesLegacyAmount() {
		minor, err := c.legacyCreateAmount(request.AmountMinor)
		if err != nil {
			return nil, "", err
		}
		delete(body, "amount")
		body["amount_minor"] = minor
	}
	if c.legacyPaymentNo != "" {
		body["payment_no"] = c.legacyPaymentNo
	}
	if c.notifyEnabled != nil {
		body["notify_enabled"] = *c.notifyEnabled
	}
	if c.notifyURL != "" && c.notifyMode != "merchant_default" {
		body["notify_url"], body["notify_secret"] = c.notifyURL, c.notifySecret
	}
	return body, returnURL, nil
}
