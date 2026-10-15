package runtime

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestHistoricalExpiredNotificationUsesFrozenAuthorityAndRecordsLateReceipt(t *testing.T) {
	const oldSecret = "original-notification-secret-1234567890"
	const newSecret = "replacement-notification-secret-123456"
	var oldPaid, newPaid atomic.Bool
	oldPaid.Store(true)
	var lookups atomic.Int32
	occurred := time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			json.NewEncoder(w).Encode(map[string]any{"access_token": r.FormValue("client_secret"), "expires_in": 3600})
			return
		}
		id := r.URL.Path[len("/openapi/v1/merchant-payments/"):]
		paid := oldPaid.Load()
		secret := "old-client"
		if id == "mp-new" {
			paid = newPaid.Load()
			secret = "new-client"
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(403)
			return
		}
		lookups.Add(1)
		state := "expired"
		version := 2
		if paid {
			state, version = "paid", 3
		}
		json.NewEncoder(w).Encode(map[string]any{"order_no": id, "external_reference": "zb-42", "amount": "10.50", "currency": "USD", "status": state, "status_version": version, "event_id": "event-" + id, "provider_trade_no": "trade-" + id, "occurred_at": occurred})
	}))
	defer provider.Close()
	snapshot := func(secret, client string) []byte {
		settings, _ := json.Marshal(tdcloud.Settings{AmountMetadata: &tdcloud.AmountMetadata{Version: 2, Currency: "USD"}, APIURL: provider.URL, ClientID: "merchant", ClientSecret: client, NotifyURL: "https://board.example.test" + tdcloud.NotificationPath, NotifySecret: secret})
		raw, _ := snapshotChannel(channelConfig{Type: "tdcloud", Settings: settings})
		return raw
	}
	old := payment.Attempt{AccountID: "buyer", OrderID: 42, OrderTradeNo: "zb-42", AmountMinor: 1050, Currency: "USD", ChannelID: "old-channel", ProviderOrderID: "mp-old", ChannelSnapshot: snapshot(oldSecret, "old-client"), State: "failed", ProviderVersion: 2}
	current := old
	current.ChannelID = "new-channel"
	current.ProviderOrderID = "mp-new"
	current.ChannelSnapshot = snapshot(newSecret, "new-client")
	current.State = "pending"
	current.Revision = 1
	current.ProviderVersion = 1
	current.History = []payment.Attempt{old}
	current.PreviousOrders = []string{"mp-old"}
	attempts := &checkoutAttempts{attempt: current}
	facts := &checkoutFacts{events: map[string]bool{}}
	bind := func() *Server {
		s := &Server{httpClient: provider.Client()}
		if _, err := s.BindCheckout(CheckoutPorts{Orders: checkoutOrder{}, Facts: facts, Attempts: attempts, OrderReturnURL: checkoutReturnURL}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	server := bind()
	deliver := func(id, state, secret string, version int) *pluginv1.HTTPResponse {
		now := time.Now().UTC().Truncate(time.Second)
		timestamp := strconv.FormatInt(now.Unix(), 10)
		body, _ := json.Marshal(map[string]any{"id": "notice-" + id, "type": "merchant_payment.status_changed", "api_version": 1, "created_at": now, "data": map[string]any{"order_no": id, "external_reference": "zb-42", "amount": "10.50", "currency": "USD", "status": state, "status_version": version}})
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(timestamp + "."))
		mac.Write(body)
		r := httptest.NewRequest("POST", "https://board.example.test"+tdcloud.NotificationPath, nil)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-TDCloud-Event-ID", "notice-"+id)
		r.Header.Set("X-TDCloud-Timestamp", timestamp)
		r.Header.Set("X-TDCloud-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		request := pluginv1.NewHTTPRequest(r, body)
		request.RouteId = tdcloudNotificationRouteID
		out, err := server.HandleHTTP(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := deliver("mp-old", "paid", newSecret, 3); out.Status != 400 || lookups.Load() != 0 {
		t.Fatal("current secret authenticated historical callback")
	}
	for range 2 {
		if out := deliver("mp-old", "paid", oldSecret, 3); out.Status != 200 {
			t.Fatalf("historical callback rejected: %+v", out)
		}
	}
	a, _ := attempts.Get(t.Context(), "buyer", 42)
	if a.History[0].FactState != "recorded" || a.History[0].ProviderVersion != 3 || a.History[0].VerifiedFact == nil || len(facts.events) != 1 {
		t.Fatalf("late historical receipt lost: %+v", a)
	}
	oldPaid.Store(false)
	if out := deliver("mp-old", "expired", oldSecret, 2); out.Status != 200 {
		t.Fatal("old version rejected after paid")
	}
	a, _ = attempts.Get(t.Context(), "buyer", 42)
	if a.History[0].State != "paid" {
		t.Fatal("old expiry regressed paid")
	}
	newPaid.Store(true)
	if out := deliver("mp-new", "paid", newSecret, 3); out.Status != 200 {
		t.Fatal("extra payment silently dropped")
	}
	a, _ = attempts.Get(t.Context(), "buyer", 42)
	if a.VerifiedFact == nil || a.FactState != "review" || a.ReviewReason != "additional_payment" || len(facts.events) != 1 {
		t.Fatalf("additional payment not retained for review: %+v", a)
	}
	raw, _ := json.Marshal(a)
	var restored payment.Attempt
	json.Unmarshal(raw, &restored)
	attempts = &checkoutAttempts{attempt: restored}
	server = bind()
	if out := deliver("mp-new", "paid", newSecret, 3); out.Status != 200 || len(facts.events) != 1 {
		t.Fatal("restart replay repeated host payment")
	}
}
