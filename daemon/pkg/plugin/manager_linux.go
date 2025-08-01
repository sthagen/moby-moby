package plugin

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/log"
	"github.com/docker/docker/daemon/initlayer"
	"github.com/docker/docker/daemon/internal/stringid"
	v2 "github.com/docker/docker/daemon/pkg/plugin/v2"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/plugins"
	"github.com/moby/moby/api/types"
	"github.com/moby/sys/mount"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

func (pm *Manager) enable(p *v2.Plugin, c *controller, force bool) error {
	p.Rootfs = filepath.Join(pm.config.Root, p.PluginObj.ID, "rootfs")
	if p.IsEnabled() && !force {
		return errors.Wrap(enabledError(p.Name()), "plugin already enabled")
	}
	spec, err := p.InitSpec(pm.config.ExecRoot)
	if err != nil {
		return err
	}

	c.restart = true
	c.exitChan = make(chan bool)

	pm.mu.Lock()
	pm.cMap[p] = c
	pm.mu.Unlock()

	var propRoot string
	if p.PluginObj.Config.PropagatedMount != "" {
		propRoot = filepath.Join(filepath.Dir(p.Rootfs), "propagated-mount")

		if err := os.MkdirAll(propRoot, 0o755); err != nil {
			log.G(context.TODO()).Errorf("failed to create PropagatedMount directory at %s: %v", propRoot, err)
		}

		if err := mount.MakeRShared(propRoot); err != nil {
			return errors.Wrap(err, "error setting up propagated mount dir")
		}
	}

	rootFS := filepath.Join(pm.config.Root, p.PluginObj.ID, rootFSFileName)
	if err := initlayer.Setup(rootFS, 0, 0); err != nil {
		return errors.WithStack(err)
	}

	stdout, stderr := makeLoggerStreams(p.GetID())
	if err := pm.executor.Create(p.GetID(), *spec, stdout, stderr); err != nil {
		if p.PluginObj.Config.PropagatedMount != "" {
			if err := mount.Unmount(propRoot); err != nil {
				log.G(context.TODO()).WithField("plugin", p.Name()).WithError(err).Warn("Failed to unmount vplugin propagated mount root")
			}
		}
		return errors.WithStack(err)
	}
	return pm.pluginPostStart(p, c)
}

func (pm *Manager) pluginPostStart(p *v2.Plugin, c *controller) error {
	sockAddr := filepath.Join(pm.config.ExecRoot, p.GetID(), p.GetSocket())
	p.SetTimeout(time.Duration(c.timeoutInSecs) * time.Second)
	addr := &net.UnixAddr{Net: "unix", Name: sockAddr}
	p.SetAddr(addr)

	if p.Protocol() == plugins.ProtocolSchemeHTTPV1 {
		client, err := plugins.NewClientWithTimeout(addr.Network()+"://"+addr.String(), nil, p.Timeout())
		if err != nil {
			c.restart = false
			shutdownPlugin(p, c.exitChan, pm.executor)
			return errors.WithStack(err)
		}

		p.SetPClient(client) //nolint:staticcheck // FIXME(thaJeztah): p.SetPClient is deprecated: Hardcoded plugin client is deprecated
	}

	// Initial sleep before net Dial to allow plugin to listen on socket.
	time.Sleep(500 * time.Millisecond)
	maxRetries := 3
	var retries int
	for {
		// net dial into the unix socket to see if someone's listening.
		conn, err := net.Dial("unix", sockAddr)
		if err == nil {
			conn.Close()
			break
		}

		time.Sleep(3 * time.Second)
		retries++

		if retries > maxRetries {
			log.G(context.TODO()).Debugf("error net dialing plugin: %v", err)
			c.restart = false
			// While restoring plugins, we need to explicitly set the state to disabled
			pm.config.Store.SetState(p, false)
			shutdownPlugin(p, c.exitChan, pm.executor)
			return err
		}
	}
	pm.config.Store.SetState(p, true)
	pm.config.Store.CallHandler(p)

	return pm.save(p)
}

func (pm *Manager) restore(p *v2.Plugin, c *controller) error {
	stdout, stderr := makeLoggerStreams(p.GetID())
	alive, err := pm.executor.Restore(p.GetID(), stdout, stderr)
	if err != nil {
		return err
	}

	if pm.config.LiveRestoreEnabled {
		if !alive {
			return pm.enable(p, c, true)
		}

		c.exitChan = make(chan bool)
		c.restart = true
		pm.mu.Lock()
		pm.cMap[p] = c
		pm.mu.Unlock()
		return pm.pluginPostStart(p, c)
	}

	if alive {
		// TODO(@cpuguy83): Should we always just re-attach to the running plugin instead of doing this?
		c.restart = false
		shutdownPlugin(p, c.exitChan, pm.executor)
	}

	return nil
}

const shutdownTimeout = 10 * time.Second

func shutdownPlugin(p *v2.Plugin, ec chan bool, executor Executor) {
	pluginID := p.GetID()

	if err := executor.Signal(pluginID, unix.SIGTERM); err != nil {
		log.G(context.TODO()).Errorf("Sending SIGTERM to plugin failed with error: %v", err)
		return
	}

	timeout := time.NewTimer(shutdownTimeout)
	defer timeout.Stop()

	select {
	case <-ec:
		log.G(context.TODO()).Debug("Clean shutdown of plugin")
	case <-timeout.C:
		log.G(context.TODO()).Debug("Force shutdown plugin")
		if err := executor.Signal(pluginID, unix.SIGKILL); err != nil {
			log.G(context.TODO()).Errorf("Sending SIGKILL to plugin failed with error: %v", err)
		}

		timeout.Reset(shutdownTimeout)

		select {
		case <-ec:
			log.G(context.TODO()).Debug("SIGKILL plugin shutdown")
		case <-timeout.C:
			log.G(context.TODO()).WithField("plugin", p.Name).Warn("Force shutdown plugin FAILED")
		}
	}
}

func (pm *Manager) disable(p *v2.Plugin, c *controller) error {
	if !p.IsEnabled() {
		return errors.Wrap(errDisabled(p.Name()), "plugin is already disabled")
	}

	c.restart = false
	shutdownPlugin(p, c.exitChan, pm.executor)
	pm.config.Store.SetState(p, false)
	return pm.save(p)
}

// Shutdown stops all plugins and called during daemon shutdown.
func (pm *Manager) Shutdown() {
	plugins := pm.config.Store.GetAll()
	for _, p := range plugins {
		pm.mu.RLock()
		c := pm.cMap[p]
		pm.mu.RUnlock()

		if pm.config.LiveRestoreEnabled && p.IsEnabled() {
			log.G(context.TODO()).Debug("Plugin active when liveRestore is set, skipping shutdown")
			continue
		}
		if pm.executor != nil && p.IsEnabled() {
			c.restart = false
			shutdownPlugin(p, c.exitChan, pm.executor)
		}
	}
	if err := mount.RecursiveUnmount(pm.config.Root); err != nil {
		log.G(context.TODO()).WithError(err).Warn("error cleaning up plugin mounts")
	}
}

func (pm *Manager) upgradePlugin(p *v2.Plugin, configDigest, manifestDigest digest.Digest, blobsums []digest.Digest, tmpRootFSDir string, privileges *types.PluginPrivileges) (retErr error) {
	config, err := pm.setupNewPlugin(configDigest, privileges)
	if err != nil {
		return err
	}

	pdir := filepath.Join(pm.config.Root, p.PluginObj.ID)
	orig := filepath.Join(pdir, "rootfs")

	// Make sure nothing is mounted
	// This could happen if the plugin was disabled with `-f` with active mounts.
	// If there is anything in `orig` is still mounted, this should error out.
	if err := mount.RecursiveUnmount(orig); err != nil {
		return errdefs.System(err)
	}

	backup := orig + "-old"
	if err := os.Rename(orig, backup); err != nil {
		return errors.Wrap(errdefs.System(err), "error backing up plugin data before upgrade")
	}

	defer func() {
		if retErr != nil {
			if err := os.RemoveAll(orig); err != nil {
				log.G(context.TODO()).WithError(err).WithField("dir", backup).Error("error cleaning up after failed upgrade")
				return
			}
			if err := os.Rename(backup, orig); err != nil {
				retErr = errors.Wrap(err, "error restoring old plugin root on upgrade failure")
			}
			if err := os.RemoveAll(tmpRootFSDir); err != nil && !os.IsNotExist(err) {
				log.G(context.TODO()).WithError(err).WithField("plugin", p.Name()).Errorf("error cleaning up plugin upgrade dir: %s", tmpRootFSDir)
			}
		} else {
			if err := os.RemoveAll(backup); err != nil {
				log.G(context.TODO()).WithError(err).WithField("dir", backup).Error("error cleaning up old plugin root after successful upgrade")
			}

			p.Config = configDigest
			p.Blobsums = blobsums
		}
	}()

	if err := os.Rename(tmpRootFSDir, orig); err != nil {
		return errors.Wrap(errdefs.System(err), "error upgrading")
	}

	p.PluginObj.Config = config
	p.Manifest = manifestDigest
	if err := pm.save(p); err != nil {
		return errors.Wrap(err, "error saving upgraded plugin config")
	}

	return nil
}

func (pm *Manager) setupNewPlugin(configDigest digest.Digest, privileges *types.PluginPrivileges) (types.PluginConfig, error) {
	configRA, err := pm.blobStore.ReaderAt(context.TODO(), ocispec.Descriptor{Digest: configDigest})
	if err != nil {
		return types.PluginConfig{}, err
	}
	defer configRA.Close()

	configR := content.NewReader(configRA)

	var config types.PluginConfig
	dec := json.NewDecoder(configR)
	if err := dec.Decode(&config); err != nil {
		return types.PluginConfig{}, errors.Wrapf(err, "failed to parse config")
	}
	if dec.More() {
		return types.PluginConfig{}, errors.New("invalid config json")
	}

	requiredPrivileges := computePrivileges(config)
	if privileges != nil {
		if err := validatePrivileges(requiredPrivileges, *privileges); err != nil {
			return types.PluginConfig{}, err
		}
	}

	return config, nil
}

// createPlugin creates a new plugin. take lock before calling.
func (pm *Manager) createPlugin(name string, configDigest, manifestDigest digest.Digest, blobsums []digest.Digest, rootFSDir string, privileges *types.PluginPrivileges, opts ...CreateOpt) (_ *v2.Plugin, retErr error) {
	if err := pm.config.Store.validateName(name); err != nil { // todo: this check is wrong. remove store
		return nil, errdefs.InvalidParameter(err)
	}

	config, err := pm.setupNewPlugin(configDigest, privileges)
	if err != nil {
		return nil, err
	}

	p := &v2.Plugin{
		PluginObj: types.Plugin{
			Name:   name,
			ID:     stringid.GenerateRandomID(),
			Config: config,
		},
		Config:   configDigest,
		Blobsums: blobsums,
		Manifest: manifestDigest,
	}
	p.InitEmptySettings()
	for _, o := range opts {
		o(p)
	}

	pdir := filepath.Join(pm.config.Root, p.PluginObj.ID)
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		return nil, errors.Wrapf(err, "failed to mkdir %v", pdir)
	}

	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(pdir)
		}
	}()

	if err := os.Rename(rootFSDir, filepath.Join(pdir, rootFSFileName)); err != nil {
		return nil, errors.Wrap(err, "failed to rename rootfs")
	}

	if err := pm.save(p); err != nil {
		return nil, err
	}

	pm.config.Store.Add(p) // todo: remove

	return p, nil
}

func recursiveUnmount(target string) error {
	return mount.RecursiveUnmount(target)
}
