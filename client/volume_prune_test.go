package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/volume"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestVolumePrune(t *testing.T) {
	const expectedURL = "/volumes/prune"

	tests := []struct {
		doc             string
		opts            VolumePruneOptions
		expectedError   string
		expectedFilters string
	}{
		{
			doc: "empty options",
		},
		{
			doc: "all filter",
			opts: VolumePruneOptions{
				Filters: make(Filters).Add("all", "true"),
			},
			expectedFilters: `{"all":{"true":true}}`,
		},
		{
			doc: "all option",
			opts: VolumePruneOptions{
				All: true,
			},
			expectedFilters: `{"all":{"true":true}}`,
		},
		{
			doc: "label filters",
			opts: VolumePruneOptions{
				Filters: make(Filters).Add("label", "label1", "label2"),
			},
			expectedFilters: `{"label":{"label1":true,"label2":true}}`,
		},
		{
			doc: "all and label filters",
			opts: VolumePruneOptions{
				All:     true,
				Filters: make(Filters).Add("label", "label1", "label2"),
			},
			expectedFilters: `{"all":{"true":true},"label":{"label1":true,"label2":true}}`,
		},
		{
			doc: "conflicting options",
			opts: VolumePruneOptions{
				All:     true,
				Filters: make(Filters).Add("all", "true"),
			},
			expectedError: `conflicting options: cannot specify both "all" and "all" filter`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			client, err := NewClientWithOpts(WithMockClient(func(req *http.Request) (*http.Response, error) {
				if err := assertRequest(req, http.MethodPost, expectedURL); err != nil {
					return nil, err
				}
				query := req.URL.Query()
				actualFilters := query.Get("filters")
				if actualFilters != tc.expectedFilters {
					return nil, fmt.Errorf("filters not set in URL query properly. Expected '%s', got %s", tc.expectedFilters, actualFilters)
				}
				content, err := json.Marshal(volume.PruneReport{
					VolumesDeleted: []string{"volume"},
					SpaceReclaimed: 12345,
				})
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(content)),
				}, nil
			}))
			assert.NilError(t, err)

			_, err = client.VolumesPrune(t.Context(), tc.opts)
			if tc.expectedError != "" {
				assert.Check(t, is.ErrorType(err, errdefs.IsInvalidArgument))
				assert.Check(t, is.Error(err, tc.expectedError))
			} else {
				assert.NilError(t, err)
			}
		})
	}
}
