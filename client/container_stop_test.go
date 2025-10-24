package client

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestContainerStopError(t *testing.T) {
	client, err := NewClientWithOpts(WithMockClient(errorMock(http.StatusInternalServerError, "Server error")))
	assert.NilError(t, err)
	err = client.ContainerStop(context.Background(), "container_id", ContainerStopOptions{})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInternal))

	err = client.ContainerStop(context.Background(), "", ContainerStopOptions{})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
	assert.Check(t, is.ErrorContains(err, "value is empty"))

	err = client.ContainerStop(context.Background(), "    ", ContainerStopOptions{})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
	assert.Check(t, is.ErrorContains(err, "value is empty"))
}

// TestContainerStopConnectionError verifies that connection errors occurring
// during API-version negotiation are not shadowed by API-version errors.
//
// Regression test for https://github.com/docker/cli/issues/4890
func TestContainerStopConnectionError(t *testing.T) {
	client, err := NewClientWithOpts(WithAPIVersionNegotiation(), WithHost("tcp://no-such-host.invalid"))
	assert.NilError(t, err)

	err = client.ContainerStop(context.Background(), "container_id", ContainerStopOptions{})
	assert.Check(t, is.ErrorType(err, IsErrConnectionFailed))
}

func TestContainerStop(t *testing.T) {
	const expectedURL = "/containers/container_id/stop"
	client, err := NewClientWithOpts(WithMockClient(func(req *http.Request) (*http.Response, error) {
		if err := assertRequest(req, http.MethodPost, expectedURL); err != nil {
			return nil, err
		}
		s := req.URL.Query().Get("signal")
		if s != "SIGKILL" {
			return nil, fmt.Errorf("signal not set in URL query. Expected 'SIGKILL', got '%s'", s)
		}
		t := req.URL.Query().Get("t")
		if t != "100" {
			return nil, fmt.Errorf("t (timeout) not set in URL query properly. Expected '100', got %s", t)
		}
		return mockResponse(http.StatusOK, nil, "")(req)
	}))
	assert.NilError(t, err)
	timeout := 100
	err = client.ContainerStop(context.Background(), "container_id", ContainerStopOptions{
		Signal:  "SIGKILL",
		Timeout: &timeout,
	})
	assert.NilError(t, err)
}
