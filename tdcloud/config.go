package tdcloud

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// NotificationPath is the signed-manifest route registered by this plugin.
const NotificationPath = "/.well-known/zboard-payments/v1/tdcloud-notify"

// Settings are TDCloud-owned; the generic plugin config carries them opaquely.
type Settings struct {
	AmountMetadata *AmountMetadata `json:"_amount,omitempty"`
	APIURL         string          `json:"api_url"`
	ClientID       string          `json:"client_id"`
	ClientSecret   string          `json:"client_secret"`
	PaymentNo      string          `json:"payment_no,omitempty"` // historical snapshots only
	Currencies     map[string]int  `json:"currencies,omitempty"` // ignored legacy configuration; never used for new orders
	NotifyURL      string          `json:"notify_url,omitempty"`
	NotifySecret   string          `json:"notify_secret,omitempty"`
	// Empty denotes legacy per-order overrides. New UI configuration inherits
	// TDCloud merchant defaults and stores its platform-issued key for verification.
	NotifyMode string `json:"notify_mode,omitempty"`
	// Nil is retained only in checkout snapshots created before notification
	// support. New configuration always pins the choice for idempotent retries.
	NotifyEnabled *bool `json:"notify_enabled,omitempty"`
	// Nil delegates to TDCloud's 1800-second default. Snapshots pin an explicit
	// choice so configuration edits cannot extend an existing payment attempt.
	ExpiresIn *int64 `json:"expires_in,omitempty"`
}

func decodeSettings(raw json.RawMessage) (Settings, error) {
	var settings Settings
	if len(raw) == 0 {
		return settings, errors.New("TDCloud settings are required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return settings, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return settings, errors.New("trailing TDCloud settings data")
	}
	return settings, nil
}

func NormalizeSettings(raw, previous json.RawMessage) (json.RawMessage, error) {
	settings, err := decodeSettings(raw)
	if err != nil {
		return nil, err
	}
	if settings.AmountMetadata != nil {
		return nil, errors.New("payment amount metadata is managed by the plugin")
	}
	settings.NotifyURL = strings.TrimSpace(settings.NotifyURL)
	if settings.ClientSecret == "" && len(previous) > 0 {
		old, err := decodeSettings(previous)
		if err == nil && old.ClientID == settings.ClientID && old.APIURL == settings.APIURL {
			settings.ClientSecret = old.ClientSecret
		}
	}
	if settings.NotifySecret == "" && settings.NotifyURL != "" && len(previous) > 0 {
		old, err := decodeSettings(previous)
		if err == nil && old.NotifyMode == settings.NotifyMode &&
			(old.NotifyURL == settings.NotifyURL || settings.NotifyMode == "merchant_default") &&
			old.ClientID == settings.ClientID && old.APIURL == settings.APIURL {
			settings.NotifySecret = old.NotifySecret
		}
	}
	settings.PaymentNo = "" // New payments always use TDCloud's hosted cashier.
	// Retired manual units are decoded only to normalize historical config.
	// The decimal contract never uses them.
	settings.Currencies = nil
	if _, err := New("validate", settings.APIURL, settings.ClientID, settings.ClientSecret, nil); err != nil {
		return nil, err
	}
	if err := validateNotificationSettings(settings); err != nil {
		return nil, err
	}
	if err := validateExpiry(settings.ExpiresIn); err != nil {
		return nil, err
	}
	if settings.NotifyEnabled == nil {
		enabled := settings.NotifyURL != ""
		settings.NotifyEnabled = &enabled
	}
	return json.Marshal(settings)
}

func ProjectSettings(raw json.RawMessage) (json.RawMessage, error) {
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, err
	}
	delete(settings, "client_secret")
	delete(settings, "notify_secret")
	delete(settings, "_amount")
	delete(settings, "payment_no")
	delete(settings, "currencies")
	return json.Marshal(settings)
}

func NewFromSettings(id string, raw json.RawMessage, client *http.Client) (*Client, error) {
	return newFromSettings(id, raw, client, false)
}

func newFromSettings(id string, raw json.RawMessage, client *http.Client, saved bool) (*Client, error) {
	settings, err := decodeSettings(raw)
	if err != nil {
		return nil, err
	}
	if !saved && settings.AmountMetadata != nil {
		return nil, errors.New("active config contains saved metadata")
	}
	channel, err := New(id, settings.APIURL, settings.ClientID, settings.ClientSecret, client)
	if err != nil {
		return nil, err
	}
	if err := validateNotificationSettings(settings); err != nil {
		return nil, err
	}
	if err := validateExpiry(settings.ExpiresIn); err != nil {
		return nil, err
	}
	if saved {
		channel.legacyPaymentNo = settings.PaymentNo
	}
	channel.notifyURL, channel.notifySecret = settings.NotifyURL, settings.NotifySecret
	channel.notifyMode = settings.NotifyMode
	channel.expiresIn = settings.ExpiresIn
	if settings.AmountMetadata != nil {
		if !settings.AmountMetadata.valid() {
			return nil, errors.New("invalid saved amount metadata")
		}
		value := *settings.AmountMetadata
		channel.amount = &value
	}
	channel.notifyEnabled = settings.NotifyEnabled
	if channel.notifyEnabled == nil && settings.NotifyURL != "" {
		enabled := true
		channel.notifyEnabled = &enabled
	}
	return channel, nil
}

func NewFromSnapshot(id string, raw json.RawMessage, client *http.Client) (*Client, error) {
	c, err := newFromSettings(id, raw, client, true)
	if err == nil && c.amount == nil {
		c.legacyAmount = true
	}
	return c, err
}

func validateNotificationSettings(settings Settings) error {
	if settings.NotifyMode != "" && settings.NotifyMode != "merchant_default" {
		return errors.New("invalid TDCloud notification mode")
	}
	if settings.NotifyEnabled != nil && *settings.NotifyEnabled != (settings.NotifyURL != "") {
		return errors.New("TDCloud notification flag and endpoint must agree")
	}
	if settings.NotifyURL == "" && settings.NotifySecret == "" {
		return nil
	}
	u, err := url.Parse(settings.NotifyURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" ||
		u.Path != NotificationPath || u.RawQuery != "" ||
		len(settings.NotifyURL) > 2048 || strings.Contains(settings.NotifyURL, "\\") || strings.IndexFunc(settings.NotifyURL, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("invalid TDCloud notification URL")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.Contains(host, "%") {
		return errors.New("invalid TDCloud notification URL")
	}
	if ip := net.ParseIP(host); ip != nil && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return errors.New("invalid TDCloud notification URL")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid TDCloud notification URL")
		}
	}
	if len(settings.NotifySecret) < 32 || len(settings.NotifySecret) > 256 || settings.NotifySecret == settings.ClientSecret ||
		strings.IndexFunc(settings.NotifySecret, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("TDCloud notification secret must be independent and 32-256 bytes")
	}
	return nil
}
