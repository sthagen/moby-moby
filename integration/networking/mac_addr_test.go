package networking

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

// TestMACAddrOnRestart is a regression test for https://github.com/moby/moby/issues/47146
//   - Start a container, let it use a generated MAC address.
//   - Stop that container.
//   - Start a second container, it'll also use a generated MAC address.
//     (It's likely to recycle the first container's MAC address.)
//   - Restart the first container.
//     (The bug was that it kept its original MAC address, now already in-use.)
//   - Check that the two containers have different MAC addresses.
func TestMACAddrOnRestart(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	const netName = "testmacaddrs"
	network.CreateNoError(ctx, t, c, netName,
		network.WithDriver("bridge"),
		network.WithOption(bridge.BridgeName, netName))
	defer network.RemoveNoError(ctx, t, c, netName)

	const ctr1Name = "ctr1"
	id1 := container.Run(ctx, t, c,
		container.WithName(ctr1Name),
		container.WithImage("busybox:latest"),
		container.WithCmd("top"),
		container.WithNetworkMode(netName))
	defer c.ContainerRemove(ctx, id1, client.ContainerRemoveOptions{
		Force: true,
	})
	err := c.ContainerStop(ctx, ctr1Name, client.ContainerStopOptions{})
	assert.Assert(t, is.Nil(err))

	// Start a second container, giving the daemon a chance to recycle the first container's
	// IP and MAC addresses.
	const ctr2Name = "ctr2"
	id2 := container.Run(ctx, t, c,
		container.WithName(ctr2Name),
		container.WithImage("busybox:latest"),
		container.WithCmd("top"),
		container.WithNetworkMode(netName))
	defer c.ContainerRemove(ctx, id2, client.ContainerRemoveOptions{
		Force: true,
	})

	// Restart the first container.
	err = c.ContainerStart(ctx, ctr1Name, client.ContainerStartOptions{})
	assert.Assert(t, is.Nil(err))

	// Check that the containers ended up with different MAC addresses.

	ctr1Inspect := container.Inspect(ctx, t, c, ctr1Name)
	ctr1MAC := ctr1Inspect.NetworkSettings.Networks[netName].MacAddress

	ctr2Inspect := container.Inspect(ctx, t, c, ctr2Name)
	ctr2MAC := ctr2Inspect.NetworkSettings.Networks[netName].MacAddress

	assert.Check(t, ctr1MAC != ctr2MAC,
		"expected containers to have different MAC addresses; got %q for both", ctr1MAC)
}

// Check that a configured MAC address is restored after a container restart,
// and after a daemon restart.
func TestCfgdMACAddrOnRestart(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	const netName = "testcfgmacaddr"
	network.CreateNoError(ctx, t, c, netName,
		network.WithDriver("bridge"),
		network.WithOption(bridge.BridgeName, netName))
	defer network.RemoveNoError(ctx, t, c, netName)

	const wantMAC = "02:42:ac:11:00:42"
	const ctr1Name = "ctr1"
	id1 := container.Run(ctx, t, c,
		container.WithName(ctr1Name),
		container.WithImage("busybox:latest"),
		container.WithCmd("top"),
		container.WithNetworkMode(netName),
		container.WithMacAddress(netName, wantMAC))
	defer c.ContainerRemove(ctx, id1, client.ContainerRemoveOptions{
		Force: true,
	})

	inspect := container.Inspect(ctx, t, c, ctr1Name)
	gotMAC := inspect.NetworkSettings.Networks[netName].MacAddress
	assert.Check(t, is.Equal(wantMAC, gotMAC))

	startAndCheck := func() {
		t.Helper()
		err := c.ContainerStart(ctx, ctr1Name, client.ContainerStartOptions{})
		assert.Assert(t, is.Nil(err))
		inspect = container.Inspect(ctx, t, c, ctr1Name)
		gotMAC = inspect.NetworkSettings.Networks[netName].MacAddress
		assert.Check(t, is.Equal(wantMAC, gotMAC))
	}

	// Restart the container, check that the MAC address is restored.
	err := c.ContainerStop(ctx, ctr1Name, client.ContainerStopOptions{})
	assert.Assert(t, is.Nil(err))
	startAndCheck()

	// Restart the daemon, check that the MAC address is restored.
	err = c.ContainerStop(ctx, ctr1Name, client.ContainerStopOptions{})
	assert.Assert(t, is.Nil(err))
	d.Restart(t)
	startAndCheck()
}

// Regression test for https://github.com/moby/moby/issues/47228 - check that a
// generated MAC address is not included in the Config section of 'inspect'
// output, but a configured address is.
func TestInspectCfgdMAC(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := setupTest(t)

	d := daemon.New(t, daemon.WithEnvVars("DOCKER_MIN_API_VERSION=1.43"))
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	testcases := []struct {
		name       string
		desiredMAC string
		netName    string
		ctrWide    bool
	}{
		{
			name:    "generated address default bridge",
			netName: "bridge",
		},
		{
			name:       "configured address default bridge",
			desiredMAC: "02:42:ac:11:00:42",
			netName:    "bridge",
		},
		{
			name:    "generated address custom bridge",
			netName: "testnet",
		},
		{
			name:       "configured address custom bridge",
			desiredMAC: "02:42:ac:11:00:42",
			netName:    "testnet",
		},
		{
			name:       "ctr-wide address default bridge",
			desiredMAC: "02:42:ac:11:00:42",
			netName:    "bridge",
			ctrWide:    true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			var copts []client.Opt
			if tc.ctrWide {
				copts = append(copts, client.WithVersion("1.43"))
			} else {
				copts = append(copts, client.WithVersion("1.51"))
			}
			c := d.NewClientT(t, copts...)
			defer c.Close()

			if tc.netName != "bridge" {
				const netName = "inspectcfgmac"
				network.CreateNoError(ctx, t, c, netName,
					network.WithDriver("bridge"),
					network.WithOption(bridge.BridgeName, netName))
				defer network.RemoveNoError(ctx, t, c, netName)
			}

			const ctrName = "ctr"
			opts := []func(*container.TestContainerConfig){
				container.WithName(ctrName),
				container.WithCmd("top"),
				container.WithImage("busybox:latest"),
			}
			// Don't specify the network name for the bridge network, because that
			// exercises a different code path (the network name isn't set until the
			// container starts, until then it's "default").
			if tc.netName != "bridge" {
				opts = append(opts, container.WithNetworkMode(tc.netName))
			}
			var id string
			if tc.desiredMAC != "" {
				if tc.ctrWide {
					id = createLegacyContainer(ctx, t, c, tc.desiredMAC, opts...)
				} else {
					opts = append(opts, container.WithMacAddress(tc.netName, tc.desiredMAC))
					id = container.Create(ctx, t, c, opts...)
				}
			} else {
				id = container.Create(ctx, t, c, opts...)
			}
			defer c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{
				Force: true,
			})

			_, raw, err := c.ContainerInspectWithRaw(ctx, id, false)
			assert.NilError(t, err)
			var inspect struct {
				Config struct {
					// Mac Address of the container.
					//
					// MacAddress field is deprecated since API v1.44. Use EndpointSettings.MacAddress instead.
					MacAddress string `json:",omitempty"`
				}
			}
			err = json.Unmarshal(raw, &inspect)
			assert.NilError(t, err, string(raw))
			configMAC := inspect.Config.MacAddress
			assert.Check(t, is.DeepEqual(configMAC, tc.desiredMAC), string(raw))
		})
	}
}

// Regression test for https://github.com/moby/moby/issues/47441
// Migration of a container-wide MAC address to the new per-endpoint setting,
// where NetworkMode uses network id, and the key in endpoint settings is the
// network name.
func TestWatchtowerCreate(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "no macvlan")

	ctx := setupTest(t)

	d := daemon.New(t, daemon.WithEnvVars("DOCKER_MIN_API_VERSION=1.25"))
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t, client.WithVersion("1.25"))
	defer c.Close()

	// Create a "/29" network, with a single address in iprange for IPAM to
	// allocate, but no gateway address. So, the gateway will get the single
	// free address. It'll only be possible to start a container by explicitly
	// assigning an address.
	const netName = "wtmvl"
	netId := network.CreateNoError(ctx, t, c, netName,
		network.WithIPAMRange("172.30.0.0/29", "172.30.0.1/32", ""),
		network.WithDriver("macvlan"),
	)
	defer network.RemoveNoError(ctx, t, c, netName)

	// Start a container, using the network's id in NetworkMode but its name
	// in EndpointsConfig. (The container-wide MAC address must be merged with
	// the endpoint config containing the preferred IP address, but the names
	// don't match.)
	const ctrName = "ctr1"
	const ctrIP = "172.30.0.2"
	const ctrMAC = "02:42:ac:11:00:42"
	opts := []func(*container.TestContainerConfig){
		container.WithName(ctrName),
		container.WithNetworkMode(netId),
		container.WithIPv4(netName, ctrIP),
	}
	id := createLegacyContainer(ctx, t, c, ctrMAC, opts...)
	defer c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	err := c.ContainerStart(ctx, id, client.ContainerStartOptions{})
	assert.NilError(t, err)

	// Check that the container got the expected addresses.
	inspect := container.Inspect(ctx, t, c, ctrName)
	netSettings := inspect.NetworkSettings.Networks[netName]
	assert.Check(t, is.Equal(netSettings.IPAddress, netip.MustParseAddr(ctrIP)))
	assert.Check(t, is.Equal(netSettings.MacAddress, ctrMAC))
}

type legacyCreateRequest struct {
	containertypes.CreateRequest
	// Mac Address of the container.
	//
	// MacAddress field is deprecated since API v1.44. Use EndpointSettings.MacAddress instead.
	MacAddress string `json:",omitempty"`
}

func createLegacyContainer(ctx context.Context, t *testing.T, apiClient client.APIClient, desiredMAC string, ops ...func(*container.TestContainerConfig)) string {
	t.Helper()
	config := container.NewTestConfig(ops...)
	ep := "/v" + apiClient.ClientVersion() + "/containers/create"
	if config.Name != "" {
		ep += "?name=" + config.Name
	}
	res, _, err := request.Post(ctx, ep, request.Host(apiClient.DaemonHost()), request.JSONBody(&legacyCreateRequest{
		CreateRequest: containertypes.CreateRequest{
			Config:           config.Config,
			HostConfig:       config.HostConfig,
			NetworkingConfig: config.NetworkingConfig,
		},
		MacAddress: desiredMAC,
	}))
	assert.NilError(t, err)
	buf, err := request.ReadBody(res.Body)
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusCreated, string(buf))
	var resp containertypes.CreateResponse
	err = json.Unmarshal(buf, &resp)
	assert.NilError(t, err)
	return resp.ID
}
