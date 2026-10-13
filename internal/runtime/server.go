package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sync"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const pluginID = "zboard.payments"
const pluginVersion = "0.0.1"

var channelIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

type channelConfig struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Settings json.RawMessage `json:"settings"`
	Enabled  bool            `json:"enabled"`
}

type config struct {
	Channels    []channelConfig     `json:"channels"`
	Diagnostics []channelDiagnostic `json:"diagnostics,omitempty"` // read-only view metadata
}

func parseConfig(raw, previous []byte) (config, error) {
	var c, old config
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || len(raw) > 64<<10 || trimmed[0] != '{' {
		return c, errors.New("configuration must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return c, errors.New("trailing configuration data")
	}
	c.Diagnostics = nil // Browser-provided readiness is never authoritative.
	if len(previous) > 0 {
		_ = json.Unmarshal(previous, &old)
	}
	if len(c.Channels) == 0 {
		return c, nil // the unconfigured initial plugin state
	}
	if len(c.Channels) > 16 {
		return c, errors.New("configure 1-16 payment channels")
	}
	seen := map[string]bool{}
	for i := range c.Channels {
		channel := &c.Channels[i]
		provider, known := channelProviders[channel.Type]
		if !channelIDPattern.MatchString(channel.ID) || seen[channel.ID] || channel.Name == "" || len(channel.Name) > 80 || !known {
			return c, errors.New("invalid payment channel identity")
		}
		seen[channel.ID] = true
		var previousSettings json.RawMessage
		for _, previousChannel := range old.Channels {
			if previousChannel.ID == channel.ID && previousChannel.Type == channel.Type {
				previousSettings = previousChannel.Settings
				break
			}
		}
		normalized, err := provider.normalize(channel.Settings, previousSettings)
		if err != nil {
			return c, err
		}
		channel.Settings = normalized
	}
	return c, nil
}

type Server struct {
	pluginv1.UnimplementedPluginControlServer
	mu               sync.RWMutex
	config           config
	revision         uint64
	configGeneration uint64
	methodsCache     map[string]methodsCacheEntry
	methodsFlights   map[string]*methodsFlight
	channelCache     map[string]payment.Channel
	diagnostics      map[string][]channelDiagnostic
	checkout         *CheckoutCoordinator
	attempts         *storage
	host             *pluginv1.HostClient
	httpClient       *http.Client
}

func New() *Server { return &Server{} }

var pluginCapabilities = []string{"zboard.ui.page.v1", "zboard.ui.slot.v1", "zboard.config.v1", "zboard.storage.v1", "zboard.http.route.v1", pluginv1.CommerceOrdersReadCapability, pluginv1.CommercePaymentsWriteCapability}

func (*Server) GetInfo(context.Context, *pluginv1.Empty) (*pluginv1.Info, error) {
	return &pluginv1.Info{Id: pluginID, Version: pluginVersion, Protocol: pluginv1.ProtocolVersion,
		Capabilities: append([]string(nil), pluginCapabilities...)}, nil
}

func (s *Server) Health(context.Context, *pluginv1.Empty) (*pluginv1.HealthResult, error) {
	message := "payment channel runtime is ready; checkout awaits host commerce access"
	if s.readiness().CheckoutReady {
		message = "payment channel runtime and checkout are ready"
	}
	return &pluginv1.HealthResult{Healthy: true, Message: message}, nil
}

func (s *Server) ValidateConfig(_ context.Context, r *pluginv1.ConfigRequest) (*pluginv1.ConfigResult, error) {
	c, err := parseConfig(r.ConfigJson, r.PreviousConfigJson)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	raw, _ := json.Marshal(c)
	return &pluginv1.ConfigResult{NormalizedJson: raw}, nil
}

func (s *Server) ApplyConfig(_ context.Context, r *pluginv1.ConfigRequest) (*pluginv1.HealthResult, error) {
	c, err := parseConfig(r.ConfigJson, nil)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.mu.Lock()
	s.config, s.revision = c, r.Revision
	s.configGeneration++
	s.methodsCache = nil
	s.methodsFlights = nil
	s.diagnostics = nil
	s.mu.Unlock()
	return &pluginv1.HealthResult{Healthy: true, Message: "payment configuration applied"}, nil
}

func (s *Server) TestConfig(ctx context.Context, r *pluginv1.ConfigRequest) (*pluginv1.HealthResult, error) {
	c, err := parseConfig(r.ConfigJson, r.PreviousConfigJson)
	if err != nil || len(c.Channels) == 0 {
		return &pluginv1.HealthResult{Healthy: false, Message: "invalid or empty payment configuration"}, nil
	}
	return s.testChannels(ctx, c), nil
}

func (s *Server) current() config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}
