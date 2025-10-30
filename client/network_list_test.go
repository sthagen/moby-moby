package client

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestNetworkListError(t *testing.T) {
	client, err := New(WithMockClient(errorMock(http.StatusInternalServerError, "Server error")))
	assert.NilError(t, err)

	_, err = client.NetworkList(context.Background(), NetworkListOptions{})
	assert.Check(t, is.ErrorType(err, cerrdefs.IsInternal))
}

func TestNetworkList(t *testing.T) {
	const expectedURL = "/networks"

	listCases := []struct {
		options         NetworkListOptions
		expectedFilters string
	}{
		{
			options:         NetworkListOptions{},
			expectedFilters: "",
		},
		{
			options: NetworkListOptions{
				Filters: make(Filters).Add("dangling", "false"),
			},
			expectedFilters: `{"dangling":{"false":true}}`,
		},
		{
			options: NetworkListOptions{
				Filters: make(Filters).Add("dangling", "true"),
			},
			expectedFilters: `{"dangling":{"true":true}}`,
		},
		{
			options: NetworkListOptions{
				Filters: make(Filters).
					Add("label", "label1").
					Add("label", "label2"),
			},
			expectedFilters: `{"label":{"label1":true,"label2":true}}`,
		},
	}

	for _, listCase := range listCases {
		client, err := New(WithMockClient(func(req *http.Request) (*http.Response, error) {
			if err := assertRequest(req, http.MethodGet, expectedURL); err != nil {
				return nil, err
			}
			query := req.URL.Query()
			actualFilters := query.Get("filters")
			if actualFilters != listCase.expectedFilters {
				return nil, fmt.Errorf("filters not set in URL query properly. Expected '%s', got %s", listCase.expectedFilters, actualFilters)
			}
			return mockJSONResponse(http.StatusOK, nil, []network.Summary{
				{
					Network: network.Network{
						Name:   "network",
						Driver: "bridge",
					},
				},
			})(req)
		}))
		assert.NilError(t, err)

		res, err := client.NetworkList(context.Background(), listCase.options)
		assert.NilError(t, err)
		assert.Check(t, is.Len(res.Items, 1))
	}
}
