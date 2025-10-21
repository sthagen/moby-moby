package client

import (
	"context"
	"net/url"

	"github.com/moby/moby/api/types/network"
)

// NetworkInspectResult contains the result of a network inspection.
type NetworkInspectResult struct {
	Network network.Inspect
	Raw     []byte
}

// NetworkInspect returns the information for a specific network configured in the docker host.
func (cli *Client) NetworkInspect(ctx context.Context, networkID string, options NetworkInspectOptions) (NetworkInspectResult, error) {
	networkID, err := trimID("network", networkID)
	if err != nil {
		return NetworkInspectResult{}, err
	}
	query := url.Values{}
	if options.Verbose {
		query.Set("verbose", "true")
	}
	if options.Scope != "" {
		query.Set("scope", options.Scope)
	}

	resp, err := cli.get(ctx, "/networks/"+networkID, query, nil)
	defer ensureReaderClosed(resp)
	if err != nil {
		return NetworkInspectResult{}, err
	}

	var out NetworkInspectResult
	out.Raw, err = decodeWithRaw(resp, &out.Network)
	return out, err
}
