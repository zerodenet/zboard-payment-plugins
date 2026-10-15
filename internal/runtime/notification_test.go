package runtime

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	"github.com/zerodenet/zboard-pay-plugins/tdcloud"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

func TestTDCloudNotificationVerifiesAttemptThenUsesServerLookup(t *testing.T) {
	var paid atomic.Bool
	var lookups atomic.Int32
	occurred := time.Now().UTC().Truncate(time.Second)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant-token", "expires_in": 3600})
		case "/openapi/v1/merchant-payments/mp-42":
			lookups.Add(1)
			status := "pending"
			if paid.Load() {
				status = "paid"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"order_no": "mp-42", "external_reference": "zb-42", "payment_no": "stripe-usd",
				"amount": "10.50", "amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": status, "event_id": "payment-event-42",
				"provider_trade_no": "trade-42", "occurred_at": occurred})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	const secret = "independent-merchant-notification-secret-123456"
	settings, _ := json.Marshal(tdcloud.Settings{Currencies: map[string]int{"USD": 2, "CNY": 2, "TDC": 8, "JPY": 0}, APIURL: provider.URL, ClientID: "merchant-client", ClientSecret: "merchant-secret",
		PaymentNo: "stripe-usd", NotifyURL: "https://hooks.example.test" + tdcloud.NotificationPath, NotifySecret: secret})
	snapshot, _ := snapshotChannel(channelConfig{Type: "tdcloud", Settings: settings})
	attempts := &checkoutAttempts{attempt: payment.Attempt{AccountID: "buyer", OrderID: 42, OrderTradeNo: "zb-42", ChannelID: "tdcloud",
		AmountMinor: 1050, Currency: "USD", ProviderOrderID: "mp-42", ChannelSnapshot: snapshot, State: "pending", Revision: 1}}
	facts := &checkoutFacts{events: map[string]bool{}}
	server := &Server{httpClient: provider.Client()}
	checkout, err := server.BindCheckout(CheckoutPorts{Orders: checkoutOrder{}, Facts: facts, Attempts: attempts, OrderReturnURL: checkoutReturnURL})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	timestamp := strconv.FormatInt(now.Unix(), 10)
	makeEvent := func(orderNo string) []byte {
		body, _ := json.Marshal(map[string]any{"id": "notification-42", "type": "merchant_payment.status_changed", "api_version": 1, "created_at": now,
			"data": map[string]any{"order_no": orderNo, "external_reference": "zb-42", "payment_no": "stripe-usd",
				"amount": "10.50", "amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": "paid", "status_version": 2, "event_id": "payment-event-42"}})
		return body
	}
	sign := func(body []byte) string {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(timestamp + "."))
		_, _ = mac.Write(body)
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	headers := func(signature string) http.Header {
		return http.Header{"X-Tdcloud-Event-Id": {"notification-42"}, "X-Tdcloud-Timestamp": {timestamp}, "X-Tdcloud-Signature": {signature}}
	}
	body := makeEvent("mp-42")
	if _, err := checkout.ConfirmNotification(t.Context(), "buyer", 42, body, headers("sha256=invalid"), now); !errors.Is(err, tdcloud.ErrInvalidNotification) {
		t.Fatalf("unsigned event accepted: %v", err)
	}
	wrongOrder := makeEvent("mp-other")
	if _, err := checkout.ConfirmNotification(t.Context(), "buyer", 42, wrongOrder, headers(sign(wrongOrder)), now); !errors.Is(err, payment.ErrConflict) {
		t.Fatalf("unrelated order accepted: %v", err)
	}
	if _, err := checkout.ConfirmNotification(t.Context(), "buyer", 42, wrongOrder, nil, now); !errors.Is(err, tdcloud.ErrInvalidNotification) {
		t.Fatalf("unsigned notification accepted: %v", err)
	}
	if _, err := checkout.ConfirmNotification(t.Context(), "buyer", 42, body, http.Header{"X-Tdcloud-Event-Id": {"notification-42"}}, now); !errors.Is(err, tdcloud.ErrInvalidNotification) {
		t.Fatalf("partial signature headers accepted: %v", err)
	}
	if lookups.Load() != 0 {
		t.Fatal("unverified or unrelated notifications must not trigger provider lookup")
	}
	makeRouteRequest := func(body []byte, signature string) *pluginv1.HTTPRequest {
		request := httptest.NewRequest(http.MethodPost, "https://hooks.example.test"+tdcloudNotificationPath, bytes.NewReader(body))
		request.Header = headers(signature)
		request.Header.Set("Content-Type", "application/json")
		forwarded := pluginv1.NewHTTPRequest(request, body)
		forwarded.RouteId = tdcloudNotificationRouteID
		return forwarded
	}
	routeRequest := makeRouteRequest(body, sign(body))
	response, err := New().HandleHTTP(t.Context(), routeRequest)
	if err != nil || response.Status != http.StatusServiceUnavailable {
		t.Fatalf("unbound checkout must request callback retry: %+v %v", response, err)
	}
	invalidBody := []byte(`{"bad":true}`)
	invalidRequest := makeRouteRequest(invalidBody, sign(invalidBody))
	response, err = server.HandleHTTP(t.Context(), invalidRequest)
	if err != nil || response.Status != http.StatusBadRequest {
		t.Fatalf("invalid callback body: %+v %v", response, err)
	}
	unsignedRequest := makeRouteRequest(body, "")
	response, err = server.HandleHTTP(t.Context(), unsignedRequest)
	if err != nil || response.Status != http.StatusBadRequest || lookups.Load() != 0 {
		t.Fatalf("unsigned public callback: %+v %v", response, err)
	}
	invalidSignature := makeRouteRequest(body, "sha256=invalid")
	response, err = server.HandleHTTP(t.Context(), invalidSignature)
	if err != nil || response.Status != http.StatusBadRequest || lookups.Load() != 0 {
		t.Fatalf("invalid public callback signature: %+v %v", response, err)
	}
	duplicateHeader := makeRouteRequest(body, sign(body))
	duplicateHeader.Headers["x-tdcloud-signature"] = &pluginv1.HTTPHeaderValues{Values: [][]byte{[]byte(sign(body))}}
	response, err = server.HandleHTTP(t.Context(), duplicateHeader)
	if err != nil || response.Status != http.StatusBadRequest || lookups.Load() != 0 {
		t.Fatalf("duplicate signature header: %+v %v", response, err)
	}
	queryRequest := makeRouteRequest(body, sign(body))
	queryRequest.RawQuery = "unexpected=true"
	response, err = server.HandleHTTP(t.Context(), queryRequest)
	if err != nil || response.Status != http.StatusBadRequest || lookups.Load() != 0 {
		t.Fatalf("TDCloud callback query must be handled by the plugin: %+v %v", response, err)
	}
	unknownRequest := makeRouteRequest(wrongOrder, sign(wrongOrder))
	unknownRequest.RouteId = "unknown-route"
	response, err = server.HandleHTTP(t.Context(), unknownRequest)
	if err != nil || response.Status != http.StatusNotFound || lookups.Load() != 0 {
		t.Fatalf("unknown notification route: %+v %v", response, err)
	}
	unknownRequest.RouteId = tdcloudNotificationRouteID
	response, err = server.HandleHTTP(t.Context(), unknownRequest)
	if err != nil || response.Status != http.StatusBadRequest || lookups.Load() != 0 {
		t.Fatalf("unknown provider order: %+v %v", response, err)
	}
	response, err = server.HandleHTTP(t.Context(), routeRequest)
	if err != nil || response.Status != http.StatusServiceUnavailable || response.RetryAfter != 30 || lookups.Load() != 1 {
		t.Fatalf("paid hint before provider confirmation must retry: %+v %v", response, err)
	}
	view, err := checkout.ConfirmNotification(t.Context(), "buyer", 42, body, nil, now)
	if !errors.Is(err, tdcloud.ErrInvalidNotification) || lookups.Load() != 1 {
		t.Fatalf("unsigned notification must not trigger lookup: %+v %v", view, err)
	}
	view, err = checkout.ConfirmNotification(t.Context(), "buyer", 42, body, headers(sign(body)), now)
	if !errors.Is(err, payment.ErrPending) || view.State != "pending" || lookups.Load() != 2 {
		t.Fatalf("signed paid hint without paid lookup: %+v %v", view, err)
	}
	paid.Store(true)
	type confirmation struct {
		state string
		err   error
	}
	results := make(chan confirmation, 2)
	go func() {
		response, err := server.HandleHTTP(t.Context(), routeRequest)
		state := ""
		if response != nil && response.Status == http.StatusOK {
			state = "paid"
		}
		results <- confirmation{state, err}
	}()
	go func() {
		view, err := checkout.Confirm(t.Context(), "buyer", 42)
		results <- confirmation{view.State, err}
	}()
	for range 2 {
		result := <-results
		if result.err != nil || result.state != "paid" {
			t.Fatalf("concurrent notification and buyer confirmation: %+v", result)
		}
	}
	priorLookups := lookups.Load()
	view, err = checkout.ConfirmNotification(t.Context(), "buyer", 42, body, headers(sign(body)), now)
	if err != nil || view.State != "paid" || lookups.Load() != priorLookups || len(facts.events) != 1 {
		t.Fatalf("duplicate event should use settled state: %+v %v, facts=%v", view, err, facts.events)
	}
	response, err = server.HandleHTTP(t.Context(), routeRequest)
	if err != nil || response.Status != http.StatusOK || lookups.Load() != priorLookups {
		t.Fatalf("duplicate signed public callback: %+v %v", response, err)
	}
}
