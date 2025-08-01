package plugin

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/docker/docker/daemon/internal/containerfs"
	"github.com/docker/docker/daemon/internal/stringid"
	v2 "github.com/docker/docker/daemon/pkg/plugin/v2"
	"github.com/docker/docker/daemon/server/backend"
	"github.com/moby/moby/api/types"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/sys/mount"
	"github.com/moby/sys/mountinfo"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"gotest.tools/v3/skip"
)

func TestManagerWithPluginMounts(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")

	root := t.TempDir()
	t.Cleanup(func() { _ = containerfs.EnsureRemoveAll(root) })

	s := NewStore()
	managerRoot := filepath.Join(root, "manager")
	p1 := newTestPlugin(t, "test1", "testcap", managerRoot)

	p2 := newTestPlugin(t, "test2", "testcap", managerRoot)
	p2.PluginObj.Enabled = true

	m, err := NewManager(
		ManagerConfig{
			Store:          s,
			Root:           managerRoot,
			ExecRoot:       filepath.Join(root, "exec"),
			CreateExecutor: func(*Manager) (Executor, error) { return nil, nil },
			LogPluginEvent: func(_, _ string, _ events.Action) {},
		})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Add(p1); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(p2); err != nil {
		t.Fatal(err)
	}

	// Create a mount to simulate a plugin that has created it's own mounts
	p2Mount := filepath.Join(p2.Rootfs, "testmount")
	if err := os.MkdirAll(p2Mount, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mount.Mount("tmpfs", p2Mount, "tmpfs", ""); err != nil {
		t.Fatal(err)
	}

	if err := m.Remove(p1.GetID(), &backend.PluginRmConfig{ForceRemove: true}); err != nil {
		t.Fatal(err)
	}
	if mounted, err := mountinfo.Mounted(p2Mount); !mounted || err != nil {
		t.Fatalf("expected %s to be mounted, err: %v", p2Mount, err)
	}
}

func newTestPlugin(t *testing.T, name, capability, root string) *v2.Plugin {
	id := stringid.GenerateRandomID()
	rootfs := filepath.Join(root, id)
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}

	p := v2.Plugin{PluginObj: types.Plugin{ID: id, Name: name}}
	p.Rootfs = rootfs
	iType := types.PluginInterfaceType{Capability: capability, Prefix: "docker", Version: "1.0"}
	i := types.PluginConfigInterface{Socket: "plugin.sock", Types: []types.PluginInterfaceType{iType}}
	p.PluginObj.Config.Interface = i
	p.PluginObj.ID = id

	return &p
}

type simpleExecutor struct {
	Executor
}

func (e *simpleExecutor) Create(id string, spec specs.Spec, stdout, stderr io.WriteCloser) error {
	return errors.New("Create failed")
}

func TestCreateFailed(t *testing.T) {
	root, err := os.MkdirTemp("", "test-create-failed")
	if err != nil {
		t.Fatal(err)
	}
	defer containerfs.EnsureRemoveAll(root)

	s := NewStore()
	managerRoot := filepath.Join(root, "manager")
	p := newTestPlugin(t, "create", "testcreate", managerRoot)

	m, err := NewManager(
		ManagerConfig{
			Store:          s,
			Root:           managerRoot,
			ExecRoot:       filepath.Join(root, "exec"),
			CreateExecutor: func(*Manager) (Executor, error) { return &simpleExecutor{}, nil },
			LogPluginEvent: func(_, _ string, _ events.Action) {},
		})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Add(p); err != nil {
		t.Fatal(err)
	}

	if err := m.enable(p, &controller{}, false); err == nil {
		t.Fatalf("expected Create failed error, got %v", err)
	}

	if err := m.Remove(p.GetID(), &backend.PluginRmConfig{ForceRemove: true}); err != nil {
		t.Fatal(err)
	}
}

type executorWithRunning struct {
	m         *Manager
	root      string
	exitChans map[string]chan struct{}
}

func (e *executorWithRunning) Create(id string, spec specs.Spec, stdout, stderr io.WriteCloser) error {
	sockAddr := filepath.Join(e.root, id, "plugin.sock")
	ch := make(chan struct{})
	if e.exitChans == nil {
		e.exitChans = make(map[string]chan struct{})
	}
	e.exitChans[id] = ch
	listenTestPlugin(sockAddr, ch)
	return nil
}

func (e *executorWithRunning) IsRunning(id string) (bool, error) {
	return true, nil
}

func (e *executorWithRunning) Restore(id string, stdout, stderr io.WriteCloser) (bool, error) {
	return true, nil
}

func (e *executorWithRunning) Signal(id string, signal syscall.Signal) error {
	ch := e.exitChans[id]
	ch <- struct{}{}
	<-ch
	e.m.HandleExitEvent(id)
	return nil
}

func TestPluginAlreadyRunningOnStartup(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	t.Parallel()

	root, err := os.MkdirTemp("", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer containerfs.EnsureRemoveAll(root)

	for _, test := range []struct {
		desc   string
		config ManagerConfig
	}{
		{
			desc: "live-restore-disabled",
			config: ManagerConfig{
				LogPluginEvent: func(_, _ string, _ events.Action) {},
			},
		},
		{
			desc: "live-restore-enabled",
			config: ManagerConfig{
				LogPluginEvent:     func(_, _ string, _ events.Action) {},
				LiveRestoreEnabled: true,
			},
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			config := test.config
			desc := test.desc
			t.Parallel()

			p := newTestPlugin(t, desc, desc, config.Root)
			p.PluginObj.Enabled = true

			// Need a short-ish path here so we don't run into unix socket path length issues.
			config.ExecRoot, err = os.MkdirTemp("", "plugintest")

			executor := &executorWithRunning{root: config.ExecRoot}
			config.CreateExecutor = func(m *Manager) (Executor, error) { executor.m = m; return executor, nil }

			if err := executor.Create(p.GetID(), specs.Spec{}, nil, nil); err != nil {
				t.Fatal(err)
			}

			config.Root = filepath.Join(root, desc, "manager")
			if err := os.MkdirAll(filepath.Join(config.Root, p.GetID()), 0o755); err != nil {
				t.Fatal(err)
			}

			if !p.IsEnabled() {
				t.Fatal("plugin should be enabled")
			}
			if err := (&Manager{config: config}).save(p); err != nil {
				t.Fatal(err)
			}

			s := NewStore()
			config.Store = s
			if err != nil {
				t.Fatal(err)
			}
			defer containerfs.EnsureRemoveAll(config.ExecRoot)

			m, err := NewManager(config)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Shutdown()

			p = s.GetAll()[p.GetID()] // refresh `p` with what the manager knows

			if p.Client() == nil { //nolint:staticcheck // FIXME(thaJeztah): p.Client is deprecated: use p.Addr() and manually create the client
				t.Fatal("plugin client should not be nil")
			}
		})
	}
}

func listenTestPlugin(sockAddr string, exit chan struct{}) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(sockAddr), 0o755); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", sockAddr)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	go func() {
		<-exit
		l.Close()
		os.Remove(sockAddr)
		exit <- struct{}{}
	}()
	return l, nil
}
