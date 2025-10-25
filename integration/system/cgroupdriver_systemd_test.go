package system

import (
	"os"
	"testing"

	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

// hasSystemd checks whether the host was booted with systemd as its init
// system. Stolen from
// https://github.com/coreos/go-systemd/blob/176f85496f4e/util/util.go#L68
func hasSystemd() bool {
	fi, err := os.Lstat("/run/systemd/system")
	if err != nil {
		return false
	}
	return fi.IsDir()
}

// TestCgroupDriverSystemdMemoryLimit checks that container
// memory limit can be set when using systemd cgroupdriver.
// https://github.com/moby/moby/issues/35123
func TestCgroupDriverSystemdMemoryLimit(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	skip.If(t, !hasSystemd())
	t.Parallel()

	ctx := testutil.StartSpan(baseContext, t)

	d := daemon.New(t)
	c := d.NewClientT(t)

	d.StartWithBusybox(ctx, t, "--exec-opt", "native.cgroupdriver=systemd", "--iptables=false", "--ip6tables=false")
	defer d.Stop(t)

	const mem = int64(64 * 1024 * 1024) // 64 MB

	ctrID := container.Create(ctx, t, c, func(ctr *container.TestContainerConfig) {
		ctr.HostConfig.Resources.Memory = mem
	})
	defer c.ContainerRemove(ctx, ctrID, client.ContainerRemoveOptions{Force: true})

	err := c.ContainerStart(ctx, ctrID, client.ContainerStartOptions{})
	assert.NilError(t, err)

	s, err := c.ContainerInspect(ctx, ctrID, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Equal(t, s.Container.HostConfig.Memory, mem)
}
