package container

import (
	"context"
	"fmt"
	"testing"
	"time"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestContainerList(t *testing.T) {
	ctx := setupTest(t)
	apiClient := request.NewAPIClient(t)

	// remove all existing containers
	container.RemoveAll(ctx, t, apiClient)

	// create the containers
	num := 64
	containers := make([]string, num)
	for i := range num {
		id := container.Create(ctx, t, apiClient)
		defer container.Remove(ctx, t, apiClient, id, client.ContainerRemoveOptions{Force: true})
		containers[i] = id
	}

	// list them and verify correctness
	list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{All: true})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(list.Items, num))
	for i := range num {
		// container list should be ordered in descending creation order
		assert.Assert(t, is.Equal(list.Items[i].ID, containers[num-1-i]))
	}
}

func TestContainerList_Annotations(t *testing.T) {
	ctx := setupTest(t)

	annotations := map[string]string{
		"foo":                       "bar",
		"io.kubernetes.docker.type": "container",
	}
	testcases := []struct {
		apiVersion          string
		expectedAnnotations map[string]string
	}{
		{apiVersion: "1.44", expectedAnnotations: nil},
		{apiVersion: "1.46", expectedAnnotations: annotations},
	}

	for _, tc := range testcases {
		t.Run(fmt.Sprintf("run with version v%s", tc.apiVersion), func(t *testing.T) {
			apiClient := request.NewAPIClient(t, client.WithVersion(tc.apiVersion))
			id := container.Create(ctx, t, apiClient, container.WithAnnotations(annotations))
			defer container.Remove(ctx, t, apiClient, id, client.ContainerRemoveOptions{Force: true})

			list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
				All:     true,
				Filters: make(client.Filters).Add("id", id),
			})
			assert.NilError(t, err)
			assert.Assert(t, is.Len(list.Items, 1))
			assert.Equal(t, list.Items[0].ID, id)
			assert.Check(t, is.DeepEqual(list.Items[0].HostConfig.Annotations, tc.expectedAnnotations))
		})
	}
}

func TestContainerList_Filter(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	prev := container.Create(ctx, t, apiClient)
	top := container.Create(ctx, t, apiClient)
	next := container.Create(ctx, t, apiClient)

	defer func() {
		container.Remove(ctx, t, apiClient, prev, client.ContainerRemoveOptions{Force: true})
		container.Remove(ctx, t, apiClient, top, client.ContainerRemoveOptions{Force: true})
		container.Remove(ctx, t, apiClient, next, client.ContainerRemoveOptions{Force: true})
	}()

	containerIDs := func(containers []containertypes.Summary) []string {
		var entries []string
		for _, c := range containers {
			entries = append(entries, c.ID)
		}
		return entries
	}

	t.Run("since", func(t *testing.T) {
		ctx := testutil.StartSpan(ctx, t)
		list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
			All:     true,
			Filters: make(client.Filters).Add("since", top),
		})
		assert.NilError(t, err)
		assert.Check(t, is.Contains(containerIDs(list.Items), next))
	})

	t.Run("before", func(t *testing.T) {
		ctx := testutil.StartSpan(ctx, t)
		list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
			All:     true,
			Filters: make(client.Filters).Add("before", top),
		})
		assert.NilError(t, err)
		assert.Check(t, is.Contains(containerIDs(list.Items), prev))
	})
}

// TestListPlatform verifies that containers have a platform set
func TestContainerList_ImageManifestPlatform(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, !testEnv.UsingSnapshotter())

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	id := container.Create(ctx, t, apiClient)
	defer container.Remove(ctx, t, apiClient, id, client.ContainerRemoveOptions{Force: true})

	list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
		All: true,
	})
	assert.NilError(t, err)
	assert.Assert(t, len(list.Items) > 0)

	ctr := list.Items[0]
	if assert.Check(t, ctr.ImageManifestDescriptor != nil && ctr.ImageManifestDescriptor.Platform != nil) {
		// Check that at least OS and Architecture have a value. Other values
		// depend on the platform on which we're running the test.
		assert.Equal(t, ctr.ImageManifestDescriptor.Platform.OS, testEnv.DaemonInfo.OSType)
		assert.Check(t, ctr.ImageManifestDescriptor.Platform.Architecture != "")
	}
}

func pollForHealthStatusSummary(ctx context.Context, apiClient client.APIClient, containerID string, healthStatus containertypes.HealthStatus) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
			All:     true,
			Filters: make(client.Filters).Add("id", containerID),
		})
		if err != nil {
			return poll.Error(err)
		}
		total := 0
		version := apiClient.ClientVersion()
		for _, ctr := range list.Items {
			if ctr.Health == nil && versions.LessThan(version, "1.52") {
				total++
			} else if ctr.Health != nil && ctr.Health.Status == healthStatus && versions.GreaterThanOrEqualTo(version, "1.52") {
				total++
			}
		}

		if total == len(list.Items) {
			return poll.Success()
		}

		return poll.Continue("waiting for container to become %s", healthStatus)
	}
}

func TestContainerList_HealthSummary(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME")
	ctx := setupTest(t)
	testcases := []struct {
		apiVersion string
	}{
		{apiVersion: "1.51"},
		{apiVersion: "1.52"},
	}

	for _, tc := range testcases {
		t.Run(fmt.Sprintf("run with version v%s", tc.apiVersion), func(t *testing.T) {
			apiClient := request.NewAPIClient(t, client.WithVersion(tc.apiVersion))

			cID := container.Run(ctx, t, apiClient, container.WithTty(true), container.WithWorkingDir("/foo"), func(c *container.TestContainerConfig) {
				c.Config.Healthcheck = &containertypes.HealthConfig{
					Test:     []string{"CMD-SHELL", "if [ \"$PWD\" = \"/foo\" ]; then exit 0; else exit 1; fi;"},
					Interval: 50 * time.Millisecond,
					Retries:  3,
				}
			})

			poll.WaitOn(t, pollForHealthStatusSummary(ctx, apiClient, cID, containertypes.Healthy))
		})
	}
}
