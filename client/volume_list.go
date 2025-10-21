package client

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/moby/moby/api/types/volume"
)

// VolumeListOptions holds parameters to list volumes.
type VolumeListOptions struct {
	Filters Filters
}

// VolumeListResult holds the result from the [Client.VolumeList] method.
type VolumeListResult struct {
	Items volume.ListResponse
}

// VolumeList returns the volumes configured in the docker host.
func (cli *Client) VolumeList(ctx context.Context, options VolumeListOptions) (VolumeListResult, error) {
	query := url.Values{}

	options.Filters.updateURLValues(query)
	resp, err := cli.get(ctx, "/volumes", query, nil)
	defer ensureReaderClosed(resp)
	if err != nil {
		return VolumeListResult{}, err
	}

	var res VolumeListResult
	err = json.NewDecoder(resp.Body).Decode(&res.Items)
	return res, err
}
