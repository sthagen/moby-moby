package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/swarm"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestNodeUpdateError(t *testing.T) {
	client, err := NewClientWithOpts(WithMockClient(errorMock(http.StatusInternalServerError, "Server error")))
	assert.NilError(t, err)

	_, err = client.NodeUpdate(context.Background(), "node_id", NodeUpdateOptions{
		Version: swarm.Version{},
		Node:    swarm.NodeSpec{},
	})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInternal))

	_, err = client.NodeUpdate(context.Background(), "", NodeUpdateOptions{
		Version: swarm.Version{},
		Node:    swarm.NodeSpec{},
	})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
	assert.Check(t, is.ErrorContains(err, "value is empty"))

	_, err = client.NodeUpdate(context.Background(), "    ", NodeUpdateOptions{
		Version: swarm.Version{},
		Node:    swarm.NodeSpec{},
	})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
	assert.Check(t, is.ErrorContains(err, "value is empty"))
}

func TestNodeUpdate(t *testing.T) {
	const expectedURL = "/nodes/node_id/update"

	client, err := NewClientWithOpts(WithMockClient(func(req *http.Request) (*http.Response, error) {
		if err := assertRequest(req, http.MethodPost, expectedURL); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte("body"))),
		}, nil
	}))
	assert.NilError(t, err)

	_, err = client.NodeUpdate(context.Background(), "node_id", NodeUpdateOptions{
		Version: swarm.Version{},
		Node:    swarm.NodeSpec{},
	})
	assert.NilError(t, err)
}
