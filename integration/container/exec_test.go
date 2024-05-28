package container // import "github.com/docker/docker/integration/container"

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/integration/internal/container"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

// TestExecWithCloseStdin adds case for moby#37870 issue.
func TestExecWithCloseStdin(t *testing.T) {
	skip.If(t, testEnv.RuntimeIsWindowsContainerd(), "FIXME. Hang on Windows + containerd combination")
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()

	// run top with detached mode
	cID := container.Run(ctx, t, apiClient)

	const expected = "closeIO"
	execResp, err := apiClient.ContainerExecCreate(ctx, cID, types.ExecConfig{
		AttachStdin:  true,
		AttachStdout: true,
		Cmd:          []string{"sh", "-c", "cat && echo " + expected},
	})
	assert.NilError(t, err)

	resp, err := apiClient.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	assert.NilError(t, err)
	defer resp.Close()

	// close stdin to send EOF to cat
	assert.NilError(t, resp.CloseWrite())

	var (
		waitCh = make(chan struct{})
		resCh  = make(chan struct {
			content string
			err     error
		}, 1)
	)

	go func() {
		close(waitCh)
		defer close(resCh)
		r, err := io.ReadAll(resp.Reader)

		resCh <- struct {
			content string
			err     error
		}{
			content: string(r),
			err:     err,
		}
	}()

	<-waitCh
	select {
	case <-time.After(3 * time.Second):
		t.Fatal("failed to read the content in time")
	case got := <-resCh:
		assert.NilError(t, got.err)

		// NOTE: using Contains because no-tty's stream contains UX information
		// like size, stream type.
		assert.Assert(t, is.Contains(got.content, expected))
	}
}

func TestExec(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithTty(true), container.WithWorkingDir("/root"))

	id, err := apiClient.ContainerExecCreate(ctx, cID, types.ExecConfig{
		WorkingDir:   "/tmp",
		Env:          []string{"FOO=BAR"},
		AttachStdout: true,
		Cmd:          []string{"sh", "-c", "env"},
	})
	assert.NilError(t, err)

	inspect, err := apiClient.ContainerExecInspect(ctx, id.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(inspect.ExecID, id.ID))

	resp, err := apiClient.ContainerExecAttach(ctx, id.ID, types.ExecStartCheck{})
	assert.NilError(t, err)
	defer resp.Close()
	r, err := io.ReadAll(resp.Reader)
	assert.NilError(t, err)
	out := string(r)
	assert.NilError(t, err)
	expected := "PWD=/tmp"
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = "PWD=C:/tmp"
	}
	assert.Check(t, is.Contains(out, expected), "exec command not running in expected /tmp working directory")
	assert.Check(t, is.Contains(out, "FOO=BAR"), "exec command not running with expected environment variable FOO")
}

func TestExecUser(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME. Probably needs to wait for container to be in running state.")
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithTty(true), container.WithUser("1:1"))

	result, err := container.Exec(ctx, apiClient, cID, []string{"id"})
	assert.NilError(t, err)

	assert.Check(t, is.Contains(result.Stdout(), "uid=1(daemon) gid=1(daemon)"), "exec command not running as uid/gid 1")
}

// Test that additional groups set with `--group-add` are kept on exec when the container
// also has a user set.
// (regression test for https://github.com/moby/moby/issues/46712)
func TestExecWithGroupAdd(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME. Probably needs to wait for container to be in running state.")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithTty(true), container.WithUser("root:root"), container.WithAdditionalGroups("staff", "wheel", "audio", "777"), container.WithCmd("sleep", "5"))

	result, err := container.Exec(ctx, apiClient, cID, []string{"id"})
	assert.NilError(t, err)

	const expected = "uid=0(root) gid=0(root) groups=0(root),10(wheel),29(audio),50(staff),777"
	assert.Check(t, is.Equal(strings.TrimSpace(result.Stdout()), expected), "exec command not keeping additional groups w/ user")
}
