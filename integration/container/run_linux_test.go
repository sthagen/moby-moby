package container

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/integration/internal/container"
	net "github.com/docker/docker/integration/internal/network"
	"github.com/docker/docker/testutil"
	"github.com/docker/docker/testutil/daemon"
	"github.com/docker/go-units"
	"github.com/moby/moby/api/stdcopy"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/versions"
	"github.com/moby/moby/client"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestNISDomainname(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	// Rootless supports custom Hostname but doesn't support custom Domainname
	//  OCI runtime create failed: container_linux.go:349: starting container process caused "process_linux.go:449: container init caused \
	//  "write sysctl key kernel.domainname: open /proc/sys/kernel/domainname: permission denied\"": unknown.
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support setting Domainname (TODO: https://github.com/moby/moby/issues/40632)")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	const (
		hostname   = "foobar"
		domainname = "baz.cyphar.com"
	)

	cID := container.Run(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.Config.Hostname = hostname
		c.Config.Domainname = domainname
	})
	inspect, err := apiClient.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(hostname, inspect.Config.Hostname))
	assert.Check(t, is.Equal(domainname, inspect.Config.Domainname))

	// Check hostname.
	res, err := container.Exec(ctx, apiClient, cID,
		[]string{"cat", "/proc/sys/kernel/hostname"})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Stderr(), 0))
	assert.Equal(t, 0, res.ExitCode)
	assert.Check(t, is.Equal(hostname, strings.TrimSpace(res.Stdout())))

	// Check domainname.
	res, err = container.Exec(ctx, apiClient, cID,
		[]string{"cat", "/proc/sys/kernel/domainname"})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Stderr(), 0))
	assert.Equal(t, 0, res.ExitCode)
	assert.Check(t, is.Equal(domainname, strings.TrimSpace(res.Stdout())))
}

func TestHostnameDnsResolution(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	const (
		hostname = "foobar"
	)

	// using user defined network as we want to use internal DNS
	netName := "foobar-net"
	net.CreateNoError(ctx, t, apiClient, netName, net.WithDriver("bridge"))

	cID := container.Run(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.Config.Hostname = hostname
		c.HostConfig.NetworkMode = containertypes.NetworkMode(netName)
	})
	inspect, err := apiClient.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(hostname, inspect.Config.Hostname))

	// Clear hosts file so ping will use DNS for hostname resolution
	res, err := container.Exec(ctx, apiClient, cID,
		[]string{"sh", "-c", "echo 127.0.0.1 localhost | tee /etc/hosts && ping -c 1 foobar"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal("", res.Stderr()))
	assert.Equal(t, 0, res.ExitCode)
}

func TestUnprivilegedPortsAndPing(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support setting net.ipv4.ping_group_range and net.ipv4.ip_unprivileged_port_start")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.Config.User = "1000:1000"
	})

	// Check net.ipv4.ping_group_range.
	res, err := container.Exec(ctx, apiClient, cID, []string{"cat", "/proc/sys/net/ipv4/ping_group_range"})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Stderr(), 0))
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, `0	2147483647`, strings.TrimSpace(res.Stdout()))

	// Check net.ipv4.ip_unprivileged_port_start.
	res, err = container.Exec(ctx, apiClient, cID, []string{"cat", "/proc/sys/net/ipv4/ip_unprivileged_port_start"})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Stderr(), 0))
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "0", strings.TrimSpace(res.Stdout()))
}

func TestPrivilegedHostDevices(t *testing.T) {
	// Host devices are linux only. Also it creates host devices,
	// so needs to be same host.
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	const (
		devTest         = "/dev/test"
		devRootOnlyTest = "/dev/root-only/test"
	)

	// Create Null devices.
	if err := unix.Mknod(devTest, unix.S_IFCHR|0o600, int(unix.Mkdev(1, 3))); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(devTest)
	if err := os.Mkdir(filepath.Dir(devRootOnlyTest), 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(devRootOnlyTest))
	if err := unix.Mknod(devRootOnlyTest, unix.S_IFCHR|0o600, int(unix.Mkdev(1, 3))); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(devRootOnlyTest)

	cID := container.Run(ctx, t, apiClient, container.WithPrivileged(true))

	// Check test device.
	res, err := container.Exec(ctx, apiClient, cID, []string{"ls", devTest})
	assert.NilError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Check(t, is.Equal(strings.TrimSpace(res.Stdout()), devTest))

	// Check root-only test device.
	res, err = container.Exec(ctx, apiClient, cID, []string{"ls", devRootOnlyTest})
	assert.NilError(t, err)
	if testEnv.IsRootless() {
		assert.Equal(t, 1, res.ExitCode)
		assert.Check(t, is.Contains(res.Stderr(), "No such file or directory"))
	} else {
		assert.Equal(t, 0, res.ExitCode)
		assert.Check(t, is.Equal(strings.TrimSpace(res.Stdout()), devRootOnlyTest))
	}
}

func TestRunConsoleSize(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.42"), "skip test from new feature")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient,
		container.WithTty(true),
		container.WithImage("busybox"),
		container.WithCmd("stty", "size"),
		container.WithConsoleSize(57, 123),
	)

	poll.WaitOn(t, container.IsStopped(ctx, apiClient, cID))

	out, err := apiClient.ContainerLogs(ctx, cID, containertypes.LogsOptions{ShowStdout: true})
	assert.NilError(t, err)
	defer out.Close()

	var b bytes.Buffer
	_, err = io.Copy(&b, out)
	assert.NilError(t, err)

	assert.Equal(t, strings.TrimSpace(b.String()), "123 57")
}

func TestRunWithAlternativeContainerdShim(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := testutil.StartSpan(baseContext, t)

	realShimPath, err := exec.LookPath("containerd-shim-runc-v2")
	assert.Assert(t, err)
	realShimPath, err = filepath.Abs(realShimPath)
	assert.Assert(t, err)

	shimDir := testutil.TempDir(t)
	assert.Assert(t, err)
	shimDir, err = filepath.Abs(shimDir)
	assert.Assert(t, err)
	assert.Assert(t, os.Symlink(realShimPath, filepath.Join(shimDir, "containerd-shim-realfake-v42")))

	d := daemon.New(t,
		daemon.WithEnvVars("PATH="+shimDir+":"+os.Getenv("PATH")),
		daemon.WithContainerdSocket(""), // A new containerd instance needs to be started which inherits the PATH env var defined above.
	)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	apiClient := d.NewClientT(t)

	cID := container.Run(ctx, t, apiClient,
		container.WithImage("busybox"),
		container.WithCmd("sh", "-c", `echo 'Hello, world!'`),
		container.WithRuntime("io.containerd.realfake.v42"),
	)

	poll.WaitOn(t, container.IsStopped(ctx, apiClient, cID))

	out, err := apiClient.ContainerLogs(ctx, cID, containertypes.LogsOptions{ShowStdout: true})
	assert.NilError(t, err)
	defer out.Close()

	var b bytes.Buffer
	_, err = stdcopy.StdCopy(&b, io.Discard, out)
	assert.NilError(t, err)

	assert.Equal(t, strings.TrimSpace(b.String()), "Hello, world!")

	d.Stop(t)
	d.Start(t, "--default-runtime="+"io.containerd.realfake.v42")

	cID = container.Run(ctx, t, apiClient,
		container.WithImage("busybox"),
		container.WithCmd("sh", "-c", `echo 'Hello, world!'`),
	)

	poll.WaitOn(t, container.IsStopped(ctx, apiClient, cID))

	out, err = apiClient.ContainerLogs(ctx, cID, containertypes.LogsOptions{ShowStdout: true})
	assert.NilError(t, err)
	defer out.Close()

	b.Reset()
	_, err = stdcopy.StdCopy(&b, io.Discard, out)
	assert.NilError(t, err)

	assert.Equal(t, strings.TrimSpace(b.String()), "Hello, world!")
}

func TestMacAddressIsAppliedToMainNetworkWithShortID(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := testutil.StartSpan(baseContext, t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	apiClient, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.43"))
	assert.NilError(t, err)

	n := net.CreateNoError(ctx, t, apiClient, "testnet", net.WithIPAM("192.168.101.0/24", "192.168.101.1"))

	cid := container.Run(ctx, t, apiClient,
		container.WithImage("busybox:latest"),
		container.WithCmd("/bin/sleep", "infinity"),
		container.WithStopSignal("SIGKILL"),
		container.WithNetworkMode(n[:10]),
		container.WithContainerWideMacAddress("02:42:08:26:a9:55"))
	defer container.Remove(ctx, t, apiClient, cid, containertypes.RemoveOptions{Force: true})

	c := container.Inspect(ctx, t, apiClient, cid)
	assert.Equal(t, c.NetworkSettings.Networks["testnet"].MacAddress, "02:42:08:26:a9:55")
}

func TestStaticIPOutsideSubpool(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := testutil.StartSpan(baseContext, t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	apiClient, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.43"))
	assert.NilError(t, err)

	const netname = "subnet-range"
	n := net.CreateNoError(ctx, t, apiClient, netname, net.WithIPAMRange("10.42.0.0/16", "10.42.128.0/24", "10.42.0.1"))
	defer net.RemoveNoError(ctx, t, apiClient, n)

	cID := container.Run(ctx, t, apiClient,
		container.WithImage("busybox:latest"),
		container.WithCmd("sh", "-c", `ip -4 -oneline addr show eth0`),
		container.WithNetworkMode(netname),
		container.WithIPv4(netname, "10.42.1.3"),
	)

	poll.WaitOn(t, container.IsStopped(ctx, apiClient, cID))

	out, err := apiClient.ContainerLogs(ctx, cID, containertypes.LogsOptions{ShowStdout: true})
	assert.NilError(t, err)
	defer out.Close()

	var b bytes.Buffer
	_, err = io.Copy(&b, out)
	assert.NilError(t, err)

	assert.Check(t, is.Contains(b.String(), "inet 10.42.1.3/16"))
}

func TestWorkingDirNormalization(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	for _, tc := range []struct {
		name    string
		workdir string
	}{
		{name: "trailing slash", workdir: "/tmp/"},
		{name: "no trailing slash", workdir: "/tmp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cID := container.Run(ctx, t, apiClient,
				container.WithImage("busybox"),
				container.WithWorkingDir(tc.workdir),
			)

			defer container.Remove(ctx, t, apiClient, cID, containertypes.RemoveOptions{Force: true})

			inspect := container.Inspect(ctx, t, apiClient, cID)

			assert.Check(t, is.Equal(inspect.Config.WorkingDir, "/tmp"))
		})
	}
}

func TestSeccomp(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	const confined = `{
 "defaultAction": "SCMP_ACT_ALLOW",
 "syscalls": [ { "names": [ "chown", "chown32", "fchownat" ], "action": "SCMP_ACT_ERRNO" } ]
}
`
	type testCase struct {
		ops              []func(*container.TestContainerConfig)
		expectedExitCode int
	}
	testCases := []testCase{
		{
			ops:              nil,
			expectedExitCode: 0,
		},
		{
			ops:              []func(*container.TestContainerConfig){container.WithPrivileged(true)},
			expectedExitCode: 0,
		},
		{
			ops:              []func(*container.TestContainerConfig){container.WithSecurityOpt("seccomp=" + confined)},
			expectedExitCode: 1,
		},
		{
			// A custom profile should be still enabled, even when --privileged is set
			// https://github.com/moby/moby/issues/47499
			ops:              []func(*container.TestContainerConfig){container.WithPrivileged(true), container.WithSecurityOpt("seccomp=" + confined)},
			expectedExitCode: 1,
		},
	}
	for _, tc := range testCases {
		cID := container.Run(ctx, t, apiClient, tc.ops...)
		res, err := container.Exec(ctx, apiClient, cID, []string{"chown", "42", "/bin/true"})
		assert.NilError(t, err)
		assert.Equal(t, tc.expectedExitCode, res.ExitCode)
		if tc.expectedExitCode != 0 {
			assert.Check(t, is.Contains(res.Stderr(), "Operation not permitted"))
		}
	}
}

func TestCgroupRW(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRootless, "can't test writable cgroups in rootless (permission denied)")
	skip.If(t, testEnv.IsUserNamespace, "can't test writable cgroups in user namespaces (permission denied)")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	type testCase struct {
		name             string
		ops              []func(*container.TestContainerConfig)
		expectedErrMsg   string
		expectedExitCode int
	}
	testCases := []testCase{
		{
			name: "nil",
			ops:  nil,
			// no err msg, because disabled-by-default
			expectedExitCode: 1,
		},
		{
			name: "writable",
			ops:  []func(*container.TestContainerConfig){container.WithSecurityOpt("writable-cgroups")},
			// no err msg, because this is correct key=bool
			expectedExitCode: 0,
		},
		{
			name: "writable=true",
			ops:  []func(*container.TestContainerConfig){container.WithSecurityOpt("writable-cgroups=true")},
			// no err msg, because this is correct key=value
			expectedExitCode: 0,
		},
		{
			name: "writable=false",
			ops:  []func(*container.TestContainerConfig){container.WithSecurityOpt("writable-cgroups=false")},
			// no err msg, because this is correct key=value
			expectedExitCode: 1,
		},
		{
			name:           "writeable=true",
			ops:            []func(*container.TestContainerConfig){container.WithSecurityOpt("writeable-cgroups=true")},
			expectedErrMsg: `Error response from daemon: invalid --security-opt 2: "writeable-cgroups=true"`,
		},
		{
			name:           "writable=1",
			ops:            []func(*container.TestContainerConfig){container.WithSecurityOpt("writable-cgroups=1")},
			expectedErrMsg: `Error response from daemon: invalid --security-opt 2: "writable-cgroups=1"`,
		},
		{
			name:           "writable=potato",
			ops:            []func(*container.TestContainerConfig){container.WithSecurityOpt("writable-cgroups=potato")},
			expectedErrMsg: `Error response from daemon: invalid --security-opt 2: "writable-cgroups=potato"`,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := container.NewTestConfig(tc.ops...)
			resp, err := container.CreateFromConfig(ctx, apiClient, cfg)
			if err != nil {
				assert.Equal(t, tc.expectedErrMsg, err.Error())
				return
			}
			// TODO check if ro or not
			err = apiClient.ContainerStart(ctx, resp.ID, containertypes.StartOptions{})
			assert.NilError(t, err)

			res, err := container.Exec(ctx, apiClient, resp.ID, []string{"sh", "-ec", `
				# see also "contrib/check-config.sh" for the same test
				if [ "$(stat -f -c %t /sys/fs/cgroup 2> /dev/null)" = '63677270' ]; then
					# nice, must be cgroupsv2
					exec mkdir /sys/fs/cgroup/foo
				else
					# boo, must be cgroupsv1
					exec mkdir /sys/fs/cgroup/pids/foo
				fi
			`})
			assert.NilError(t, err)
			if tc.expectedExitCode != 0 {
				assert.Check(t, is.Contains(res.Stderr(), "Read-only file system"))
			} else {
				assert.Equal(t, res.Stderr(), "")
			}
			assert.Equal(t, res.Stdout(), "")
			assert.Equal(t, tc.expectedExitCode, res.ExitCode)
		})
	}
}

func TestContainerShmSize(t *testing.T) {
	ctx := setupTest(t)

	const defaultSize = "1000k"
	defaultSizeBytes, err := units.RAMInBytes(defaultSize)
	assert.NilError(t, err)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, "--default-shm-size="+defaultSize)
	defer d.Stop(t)

	apiClient := d.NewClientT(t)

	tests := []struct {
		doc     string
		opt     container.ConfigOpt
		expSize string
		expErr  string
	}{
		{
			doc:     "nil hostConfig",
			opt:     container.WithHostConfig(nil),
			expSize: defaultSize,
		},
		{
			doc:     "empty hostConfig",
			opt:     container.WithHostConfig(&containertypes.HostConfig{}),
			expSize: defaultSize,
		},
		{
			doc:     "custom shmSize",
			opt:     container.WithHostConfig(&containertypes.HostConfig{ShmSize: defaultSizeBytes * 2}),
			expSize: "2000k",
		},
		{
			doc:    "negative shmSize",
			opt:    container.WithHostConfig(&containertypes.HostConfig{ShmSize: -1}),
			expErr: "Error response from daemon: SHM size can not be less than 0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			if tc.expErr != "" {
				cfg := container.NewTestConfig(container.WithCmd("sh", "-c", "grep /dev/shm /proc/self/mountinfo"), tc.opt)
				_, err := container.CreateFromConfig(ctx, apiClient, cfg)
				assert.Check(t, is.ErrorContains(err, tc.expErr))
				assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
				return
			}

			cID := container.Run(ctx, t, apiClient,
				container.WithCmd("sh", "-c", "grep /dev/shm /proc/self/mountinfo"),
				tc.opt,
			)

			t.Cleanup(func() {
				container.Remove(ctx, t, apiClient, cID, containertypes.RemoveOptions{})
			})

			expectedSize, err := units.RAMInBytes(tc.expSize)
			assert.NilError(t, err)

			ctr := container.Inspect(ctx, t, apiClient, cID)
			assert.Check(t, is.Equal(ctr.HostConfig.ShmSize, expectedSize))

			out, err := container.Output(ctx, apiClient, cID)
			assert.NilError(t, err)

			// e.g., "218 213 0:87 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=1000k"
			assert.Assert(t, is.Contains(out.Stdout, "/dev/shm "), "shm mount not found in output: \n%v", out.Stdout)
			assert.Check(t, is.Contains(out.Stdout, "size="+tc.expSize))
		})
	}
}
