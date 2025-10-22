package client

import (
	"context"

	"github.com/moby/moby/api/types/swarm"
)

// SecretInspectOptions holds options for inspecting a secret.
type SecretInspectOptions struct {
	// Add future optional parameters here
}

// SecretInspectResult holds the result from the [Client.SecretInspect]. method.
type SecretInspectResult struct {
	Secret swarm.Secret
	Raw    []byte
}

// SecretInspect returns the secret information with raw data.
func (cli *Client) SecretInspect(ctx context.Context, id string, options SecretInspectOptions) (SecretInspectResult, error) {
	id, err := trimID("secret", id)
	if err != nil {
		return SecretInspectResult{}, err
	}
	resp, err := cli.get(ctx, "/secrets/"+id, nil, nil)
	defer ensureReaderClosed(resp)
	if err != nil {
		return SecretInspectResult{}, err
	}

	var out SecretInspectResult
	out.Raw, err = decodeWithRaw(resp, &out.Secret)
	return out, err
}
