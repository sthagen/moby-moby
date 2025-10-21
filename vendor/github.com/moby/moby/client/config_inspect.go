package client

import (
	"context"

	"github.com/moby/moby/api/types/swarm"
)

// ConfigInspectOptions holds options for inspecting a config.
type ConfigInspectOptions struct {
	// Add future optional parameters here
}

// ConfigInspectResult holds the result from the ConfigInspect method.
type ConfigInspectResult struct {
	Config swarm.Config
	Raw    []byte
}

// ConfigInspect returns the config information with raw data
func (cli *Client) ConfigInspect(ctx context.Context, id string, options ConfigInspectOptions) (ConfigInspectResult, error) {
	id, err := trimID("config", id)
	if err != nil {
		return ConfigInspectResult{}, err
	}
	resp, err := cli.get(ctx, "/configs/"+id, nil, nil)
	defer ensureReaderClosed(resp)
	if err != nil {
		return ConfigInspectResult{}, err
	}

	var out ConfigInspectResult
	out.Raw, err = decodeWithRaw(resp, &out.Config)
	return out, err
}
