package tdcloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func TestMerchantDefaultNotificationUsesPlatformKeyWithoutOverridingProviderConfig(t *testing.T) {
	const signingKey = "platform-notification-signing-key-123456789"
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 300})
		case "/openapi/v1/merchant-payments":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if string(body["notify_enabled"]) != "true" || body["notify_secret"] != nil || body["notify_url"] != nil {
				t.Fatal("plugin replaced the platform default notification identity")
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(merchantPayment{OrderNo: "mp", ExternalRef: "zb", Amount: "10.50", AmountMinor: 1050, Currency: "USD", CurrencyDecimals: intPointer(2), Status: "pending", CheckoutURL: "https://cashier.example.test/mp"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	enabled := true
	// Decimal snapshot uses the platform notification identity unchanged.
	raw, _ := json.Marshal(Settings{APIURL: provider.URL, ClientID: "merchant", ClientSecret: "oauth-secret", NotifyMode: "merchant_default", NotifyEnabled: &enabled, NotifyURL: "https://board.example.test" + NotificationPath, NotifySecret: signingKey, AmountMetadata: &AmountMetadata{Version: 2, Currency: "USD"}})
	client, err := NewFromSnapshot("tdcloud", raw, provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateCheckout(t.Context(), payment.CheckoutRequest{Reference: "zb", IdempotencyKey: "key", Currency: "USD", AmountMinor: 1050}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	body, _ := json.Marshal(map[string]any{"id": "notice", "type": "merchant_payment.status_changed", "api_version": 1, "created_at": now, "data": map[string]any{"order_no": "mp", "external_reference": "zb", "amount": "10.50", "amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": "paid", "status_version": 2}})
	stamp := strconv.FormatInt(now.Unix(), 10)
	headers := http.Header{"X-Tdcloud-Event-Id": {"notice"}, "X-Tdcloud-Timestamp": {stamp}, "X-Tdcloud-Signature": {signedNotification(body, stamp, signingKey)}}
	if _, err := client.InspectNotification(body, headers, now); err != nil {
		t.Fatal("platform-issued signature was rejected")
	}
	headers.Set("X-TDCloud-Signature", signedNotification(body, stamp, "another-plugin-generated-signing-key"))
	if _, err := client.InspectNotification(body, headers, now); err == nil {
		t.Fatal("accepted an unrelated replacement key")
	}
}
