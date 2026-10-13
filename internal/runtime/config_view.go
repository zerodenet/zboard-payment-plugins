package runtime

import (
	"context"
	"encoding/json"

	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DescribeConfig supports the host configuration editor without projecting
// private provider credentials. Saving omitted secrets retains prior values.
func (s *Server) DescribeConfig(_ context.Context, r *pluginv1.ConfigRequest) (*pluginv1.ConfigResult, error) {
	c, err := parseConfig(r.ConfigJson, nil)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid payment configuration")
	}
	c.Diagnostics = s.configDiagnostics(c)
	for i := range c.Channels {
		provider := channelProviders[c.Channels[i].Type]
		if provider.project == nil {
			return nil, status.Error(codes.Unavailable, "channel configuration view unavailable")
		}
		c.Channels[i].Settings, err = provider.project(c.Channels[i].Settings)
		if err != nil {
			return nil, status.Error(codes.Unavailable, "channel configuration view unavailable")
		}
	}
	raw, err := json.Marshal(c)
	return &pluginv1.ConfigResult{NormalizedJson: raw}, err
}
