package tdcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

type Client struct {
	id              string
	baseURL         string
	clientID        string
	secret          string
	legacyPaymentNo string
	notifyURL       string
	notifySecret    string
	notifyMode      string
	notifyEnabled   *bool
	expiresIn       *int64
	http            *http.Client
	auth            *merchantToken
	amount          *AmountMetadata
	legacyAmount    bool
}

type merchantToken struct {
	mu    sync.Mutex
	token string
	end   time.Time
}

type merchantPayment struct {
	OrderNo          string    `json:"order_no"`
	ExternalRef      string    `json:"external_reference"`
	ReturnURL        string    `json:"return_url"`
	PaymentNo        string    `json:"payment_no"`
	Amount           string    `json:"amount"`
	AmountMinor      int64     `json:"amount_minor"`
	Currency         string    `json:"currency"`
	CurrencyDecimals *int      `json:"currency_decimals"`
	Status           string    `json:"status"`
	StatusVersion    uint64    `json:"status_version"`
	CheckoutURL      string    `json:"checkout_url"`
	ExpiresAt        time.Time `json:"expires_at"`
	ProviderTradeNo  string    `json:"provider_trade_no"`
	EventID          string    `json:"event_id"`
	OccurredAt       time.Time `json:"occurred_at"`
}

type MerchantChannel struct {
	PaymentNo        string `json:"payment_no"`
	Name             string `json:"name"`
	Currency         string `json:"currency"`
	CurrencyDecimals *int   `json:"currency_decimals"`
	LoginRequired    bool   `json:"login_required"`
	TestMode         bool   `json:"test_mode"`
}

// APIError retains the stable TDCloud error code without exposing response bodies
// or merchant credentials to plugin logs.
type APIError struct {
	StatusCode int
	Code       string
	OAuth      bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("TDCloud API returned HTTP %d (%s)", e.StatusCode, e.Code)
}

// These formal rejection codes guarantee no new provider order was created,
// so the shared checkout may release the attempt for another payment option.
func (e *APIError) Is(target error) bool {
	if e.OAuth {
		return false
	}
	if e.StatusCode == http.StatusBadRequest && (e.Code == "invalid_amount" || e.Code == "invalid_amount_precision") {
		return target == payment.ErrAmountPrecision
	}
	return target == payment.ErrCheckoutFailed && e.StatusCode == http.StatusUnprocessableEntity && e.Code == "currency_unavailable"
}

func New(id, baseURL, clientID, secret string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" ||
		id == "" || clientID == "" || secret == "" {
		return nil, errors.New("invalid TDCloud channel configuration")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	disabled := false
	return &Client{id: id, baseURL: strings.TrimRight(baseURL, "/"), clientID: clientID, secret: secret, notifyEnabled: &disabled, http: httpClient, auth: &merchantToken{}}, nil
}

func (c *Client) ID() string { return c.id }

func (c *Client) Probe(ctx context.Context) error {
	_, err := c.cashierDirectorySize(ctx)
	return err
}

func (c *Client) cashierDirectorySize(ctx context.Context) (int, error) {
	var directory struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := c.request(ctx, http.MethodGet, "/openapi/v1/merchant-payment-channels", nil, http.StatusOK, &directory); err != nil {
		return 0, err
	}
	return len(directory.Items), nil
}

// Describe checks merchant access without choosing or projecting cashier methods.
// TDCloud validates invoice currency and decimal amounts at creation. Directory
// method currencies are never used to constrain the outer business order.
func (c *Client) Describe(ctx context.Context) (payment.ChannelInfo, error) {
	count, err := c.cashierDirectorySize(ctx)
	if err != nil {
		return payment.ChannelInfo{}, err
	}
	if count == 0 {
		return payment.ChannelInfo{}, errCashierEmpty
	}
	return payment.ChannelInfo{ValidateCurrencyAtCheckout: true}, nil
}

var _ payment.Channel = (*Client)(nil)

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.auth.mu.Lock()
	defer c.auth.mu.Unlock()
	if c.auth.token != "" && time.Until(c.auth.end) > 30*time.Second {
		return c.auth.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.clientID}, "client_secret": {c.secret},
		"scope": {"merchant_payments.create merchant_payments.read"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var remote struct {
			Code string `json:"error"`
		}
		_ = decode(response.Body, &remote)
		return "", &APIError{StatusCode: response.StatusCode, Code: remote.Code, OAuth: true}
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := decode(response.Body, &token); err != nil || token.AccessToken == "" || token.ExpiresIn <= 0 {
		return "", errors.New("invalid TDCloud OAuth response")
	}
	c.auth.token, c.auth.end = token.AccessToken, time.Now().Add(time.Duration(token.ExpiresIn)*time.Second)
	return c.auth.token, nil
}

func (c *Client) request(ctx context.Context, method, path string, body any, success int, out any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != success {
		var remote struct {
			Code string `json:"error"`
		}
		_ = decode(response.Body, &remote)
		return &APIError{StatusCode: response.StatusCode, Code: remote.Code}
	}
	return decode(response.Body, out)
}

func decode(reader io.Reader, out any) error {
	return json.NewDecoder(io.LimitReader(reader, 64<<10)).Decode(out)
}

func (c *Client) CreateCheckout(ctx context.Context, request payment.CheckoutRequest) (payment.Checkout, error) {
	if request.Reference == "" || request.Reference != strings.TrimSpace(request.Reference) || len(request.Reference) > 191 ||
		request.IdempotencyKey == "" || request.IdempotencyKey != strings.TrimSpace(request.IdempotencyKey) || len(request.IdempotencyKey) > 128 ||
		request.AmountMinor <= 0 || !validCurrency(request.Currency) {
		return payment.Checkout{}, errors.New("invalid TDCloud checkout request")
	}
	var view merchantPayment
	amount, err := formatHostAmount(request.AmountMinor)
	if err != nil {
		return payment.Checkout{}, err
	}
	if c.amount != nil && c.amount.Currency != request.Currency {
		return payment.Checkout{}, payment.ErrConflict
	}
	body, returnURL, err := c.createBody(request, amount)
	if err != nil {
		return payment.Checkout{}, err
	}
	err = c.request(ctx, http.MethodPost, "/openapi/v1/merchant-payments", body, http.StatusCreated, &view)
	if err != nil {
		return payment.Checkout{}, err
	}
	verifiedAmount, amountErr := c.resultAmount(view.Amount, view.AmountMinor, view.Currency, view.CurrencyDecimals)
	if view.OrderNo == "" || view.ExternalRef != request.Reference || (c.legacyPaymentNo != "" && view.PaymentNo != c.legacyPaymentNo) ||
		amountErr != nil || verifiedAmount != request.AmountMinor ||
		!strings.EqualFold(view.Currency, request.Currency) || view.ReturnURL != returnURL {
		return payment.Checkout{}, errors.New("TDCloud checkout does not match requested order")
	}
	if view.Status != "pending" && view.Status != "review" && view.Status != "paid" && view.Status != "expired" {
		return payment.Checkout{}, fmt.Errorf("TDCloud checkout cannot be started in state %q", view.Status)
	}
	// review and paid may be returned on an idempotent retry. Preserve the
	// provider order so the service can reconcile it by server-side lookup.
	state := view.Status
	if state == "expired" {
		state = "failed"
	}
	return payment.Checkout{ProviderOrderID: view.OrderNo, RedirectURL: view.CheckoutURL, ExpiresAt: view.ExpiresAt, Status: state, StatusVersion: view.StatusVersion}, nil
}

func (c *Client) Lookup(ctx context.Context, orderNo string) (payment.PaymentResult, error) {
	if orderNo == "" || strings.ContainsAny(orderNo, "/?&#") {
		return payment.PaymentResult{}, errors.New("invalid TDCloud order number")
	}
	var view merchantPayment
	err := c.request(ctx, http.MethodGet, "/openapi/v1/merchant-payments/"+url.PathEscape(orderNo), nil, http.StatusOK, &view)
	if err != nil {
		return payment.PaymentResult{}, err
	}
	if view.OrderNo != orderNo || (c.legacyPaymentNo != "" && view.PaymentNo != c.legacyPaymentNo) || view.ExternalRef == "" {
		return payment.PaymentResult{}, errors.New("TDCloud payment identity mismatch")
	}
	amount, err := c.resultAmount(view.Amount, view.AmountMinor, view.Currency, view.CurrencyDecimals)
	if err != nil {
		return payment.PaymentResult{}, err
	}
	var state string
	switch view.Status {
	case "pending", "paid":
		state = view.Status
	case "expired":
		state = "failed"
	case "review":
		state = "unknown"
	case "refunded", "chargeback":
		state = "reversed"
	default:
		return payment.PaymentResult{}, fmt.Errorf("unknown TDCloud payment state %q", view.Status)
	}
	return payment.PaymentResult{ProviderOrderID: view.OrderNo, Reference: view.ExternalRef, ProviderTradeNo: view.ProviderTradeNo,
		EventID: view.EventID, Status: state, StatusVersion: view.StatusVersion, ProviderState: view.Status, AmountMinor: amount,
		Currency: view.Currency, OccurredAt: view.OccurredAt}, nil
}
