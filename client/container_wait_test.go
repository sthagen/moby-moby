package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestContainerWaitError(t *testing.T) {
	client, err := NewClientWithOpts(WithMockClient(errorMock(http.StatusInternalServerError, "Server error")))
	assert.NilError(t, err)
	resultC, errC := client.ContainerWait(context.Background(), "nothing", "")
	select {
	case result := <-resultC:
		t.Fatalf("expected to not get a wait result, got %d", result.StatusCode)
	case err := <-errC:
		assert.Check(t, is.ErrorType(err, cerrdefs.IsInternal))
	}
}

// TestContainerWaitConnectionError verifies that connection errors occurring
// during API-version negotiation are not shadowed by API-version errors.
//
// Regression test for https://github.com/docker/cli/issues/4890
func TestContainerWaitConnectionError(t *testing.T) {
	client, err := NewClientWithOpts(WithAPIVersionNegotiation(), WithHost("tcp://no-such-host.invalid"))
	assert.NilError(t, err)

	resultC, errC := client.ContainerWait(context.Background(), "nothing", "")
	select {
	case result := <-resultC:
		t.Fatalf("expected to not get a wait result, got %d", result.StatusCode)
	case err := <-errC:
		assert.Check(t, is.ErrorType(err, IsErrConnectionFailed))
	}
}

func TestContainerWait(t *testing.T) {
	const expectedURL = "/containers/container_id/wait"
	client, err := NewClientWithOpts(WithMockClient(func(req *http.Request) (*http.Response, error) {
		if err := assertRequest(req, http.MethodPost, expectedURL); err != nil {
			return nil, err
		}
		b, err := json.Marshal(container.WaitResponse{
			StatusCode: 15,
		})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(b)),
		}, nil
	}))
	assert.NilError(t, err)

	resultC, errC := client.ContainerWait(context.Background(), "container_id", "")
	select {
	case err := <-errC:
		assert.NilError(t, err)
	case result := <-resultC:
		assert.Check(t, is.Equal(result.StatusCode, int64(15)))
	}
}

func TestContainerWaitProxyInterrupt(t *testing.T) {
	const (
		expectedURL = "/containers/container_id/wait"
		expErr      = "copying response body from Docker: unexpected EOF"
	)

	client, err := NewClientWithOpts(WithMockClient(func(req *http.Request) (*http.Response, error) {
		if err := assertRequest(req, http.MethodPost, expectedURL); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(expErr)),
		}, nil
	}))
	assert.NilError(t, err)

	resultC, errC := client.ContainerWait(context.Background(), "container_id", "")
	select {
	case err := <-errC:
		assert.Check(t, is.ErrorContains(err, expErr))
	case result := <-resultC:
		t.Fatalf("Unexpected result: %v", result)
	}
}

func TestContainerWaitProxyInterruptLong(t *testing.T) {
	const expectedURL = "/containers/container_id/wait"
	msg := strings.Repeat("x", containerWaitErrorMsgLimit*5)
	client, err := NewClientWithOpts(WithMockClient(func(req *http.Request) (*http.Response, error) {
		if err := assertRequest(req, http.MethodPost, expectedURL); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(msg)),
		}, nil
	}))
	assert.NilError(t, err)

	resultC, errC := client.ContainerWait(context.Background(), "container_id", "")
	select {
	case err := <-errC:
		// LimitReader limiting isn't exact, because of how the Readers do chunking.
		assert.Check(t, len(err.Error()) <= containerWaitErrorMsgLimit*2, "Expected error to be limited around %d, actual length: %d", containerWaitErrorMsgLimit, len(err.Error()))
	case result := <-resultC:
		t.Fatalf("Unexpected result: %v", result)
	}
}

func TestContainerWaitErrorHandling(t *testing.T) {
	for _, test := range []struct {
		name string
		rdr  io.Reader
		exp  error
	}{
		{name: "invalid json", rdr: strings.NewReader(`{]`), exp: errors.New("{]")},
		{name: "context canceled", rdr: iotest.ErrReader(context.Canceled), exp: context.Canceled},
		{name: "context deadline exceeded", rdr: iotest.ErrReader(context.DeadlineExceeded), exp: context.DeadlineExceeded},
		{name: "connection reset", rdr: iotest.ErrReader(syscall.ECONNRESET), exp: syscall.ECONNRESET},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			client, err := NewClientWithOpts(WithMockClient(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(test.rdr),
				}, nil
			}))
			assert.NilError(t, err)
			resultC, errC := client.ContainerWait(ctx, "container_id", "")
			select {
			case err := <-errC:
				assert.Check(t, is.Equal(err.Error(), test.exp.Error()))
				return
			case result := <-resultC:
				t.Fatalf("expected to not get a wait result, got %d", result.StatusCode)
				return
			}
			// Unexpected - we should not reach this line
		})
	}
}

func ExampleClient_ContainerWait_withTimeout() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, _ := NewClientWithOpts(FromEnv)
	_, errC := client.ContainerWait(ctx, "container_id", "")
	if err := <-errC; err != nil {
		log.Fatal(err)
	}
}
