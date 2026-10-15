package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

const (
	methodsLookupTimeout = 4 * time.Second
	methodsCacheTTL      = 60 * time.Second
)

type methodsCacheEntry struct {
	methods []Method
	expires time.Time
}

type methodsFlight struct {
	done    chan struct{}
	methods []Method
	err     error
}

// Method is the public checkout choice projected from a configured channel.
// A method ID is selected before ZBoard creates its business order.
type Method struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Currency          string     `json:"currency"`
	LoginRequired     bool       `json:"login_required"`
	TestMode          bool       `json:"test_mode"`
	Disabled          bool       `json:"disabled,omitempty"`
	CheckoutExpiresAt *time.Time `json:"checkout_expires_at,omitempty"`
}

// ListMethods is independent of the eventual ZBoard checkout slot transport.
// It checks channel directories so unavailable or wrong-currency channels
// cannot be offered to buyers. Concurrent discovery has a short deadline and
// successful results are cached per currency and configuration generation.
func (s *Server) ListMethods(ctx context.Context, currency string) ([]Method, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if len(currency) != 3 {
		return nil, fmt.Errorf("invalid checkout currency")
	}
	s.mu.Lock()
	key := fmt.Sprintf("%d:%s", s.configGeneration, currency)
	if cached, ok := s.methodsCache[key]; ok && time.Now().Before(cached.expires) {
		methods := append([]Method{}, cached.methods...)
		s.mu.Unlock()
		return methods, nil
	}
	if flight := s.methodsFlights[key]; flight != nil {
		s.mu.Unlock()
		select {
		case <-flight.done:
			return append([]Method{}, flight.methods...), flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.methodsFlights == nil {
		s.methodsFlights = make(map[string]*methodsFlight)
	}
	flight := &methodsFlight{done: make(chan struct{})}
	s.methodsFlights[key] = flight
	channels := s.config.Channels
	s.mu.Unlock()

	lookupCtx, cancel := context.WithTimeout(ctx, methodsLookupTimeout)
	methods, complete, err := s.fetchMethods(lookupCtx, channels, currency)
	cancel()

	s.mu.Lock()
	flight.methods, flight.err = methods, err
	if err == nil && complete && key == fmt.Sprintf("%d:%s", s.configGeneration, currency) {
		if s.methodsCache == nil {
			s.methodsCache = make(map[string]methodsCacheEntry)
		}
		s.methodsCache[key] = methodsCacheEntry{methods: methods, expires: time.Now().Add(methodsCacheTTL)}
	}
	delete(s.methodsFlights, key)
	close(flight.done)
	s.mu.Unlock()
	return append([]Method{}, methods...), err
}

func (s *Server) fetchMethods(ctx context.Context, channels []channelConfig, currency string) ([]Method, bool, error) {
	type candidate struct {
		config  channelConfig
		channel payment.Channel
	}
	candidates := make([]candidate, 0, len(channels))
	for _, item := range channels {
		if !item.Enabled {
			continue
		}
		channel, err := s.channelFor(item)
		if err != nil {
			return nil, false, fmt.Errorf("payment method %s: %w", item.ID, err)
		}
		candidates = append(candidates, candidate{config: item, channel: channel})
	}
	type result struct {
		index int
		info  payment.ChannelInfo
		err   error
	}
	results := make(chan result, len(candidates))
	for index, item := range candidates {
		go func() {
			info, err := item.channel.Describe(ctx)
			results <- result{index: index, info: info, err: err}
		}()
	}
	infos := make([]payment.ChannelInfo, len(candidates))
	ready := make([]bool, len(candidates))
	var firstError error
	for range candidates {
		select {
		case <-ctx.Done():
			if firstError == nil {
				firstError = ctx.Err()
			}
			goto collected
		case found := <-results:
			if found.err != nil {
				if firstError == nil {
					firstError = fmt.Errorf("payment method %s: %w", candidates[found.index].config.ID, found.err)
				}
				continue
			}
			infos[found.index] = found.info
			ready[found.index] = true
		}
	}
collected:
	methods := []Method{}
	for index, item := range candidates {
		if !ready[index] {
			continue
		}
		remote := infos[index]
		supported := remote.ValidateCurrencyAtCheckout || strings.EqualFold(remote.Currency, currency)
		for _, code := range remote.Currencies {
			supported = supported || strings.EqualFold(code, currency)
		}
		if !supported {
			continue
		}
		methods = append(methods, Method{ID: item.config.ID, Name: item.config.Name, Currency: currency,
			LoginRequired: remote.LoginRequired, TestMode: remote.TestMode})
	}
	if len(methods) > 0 || firstError == nil {
		return methods, firstError == nil, nil
	}
	return nil, false, firstError
}
