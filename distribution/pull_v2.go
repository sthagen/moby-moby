package distribution // import "github.com/docker/docker/distribution"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/distribution"
	"github.com/docker/distribution/manifest/manifestlist"
	"github.com/docker/distribution/manifest/ocischema"
	"github.com/docker/distribution/manifest/schema1"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/docker/distribution/reference"
	"github.com/docker/distribution/registry/client/transport"
	"github.com/docker/docker/distribution/metadata"
	"github.com/docker/docker/distribution/xfer"
	"github.com/docker/docker/image"
	v1 "github.com/docker/docker/image/v1"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/pkg/system"
	refstore "github.com/docker/docker/reference"
	"github.com/docker/docker/registry"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	archvariant "github.com/tonistiigi/go-archvariant"
)

var (
	errRootFSMismatch = errors.New("layers from manifest don't match image configuration")
	errRootFSInvalid  = errors.New("invalid rootfs in image configuration")
)

// imageConfigPullError is an error pulling the image config blob
// (only applies to schema2).
type imageConfigPullError struct {
	Err error
}

// Error returns the error string for imageConfigPullError.
func (e imageConfigPullError) Error() string {
	return "error pulling image configuration: " + e.Err.Error()
}

// newPuller returns a puller to pull from a v2 registry.
func newPuller(endpoint registry.APIEndpoint, repoInfo *registry.RepositoryInfo, config *ImagePullConfig, local ContentStore) *puller {
	return &puller{
		metadataService: metadata.NewV2MetadataService(config.MetadataStore),
		endpoint:        endpoint,
		config:          config,
		repoInfo:        repoInfo,
		manifestStore: &manifestStore{
			local: local,
		},
	}
}

type puller struct {
	metadataService metadata.V2MetadataService
	endpoint        registry.APIEndpoint
	config          *ImagePullConfig
	repoInfo        *registry.RepositoryInfo
	repo            distribution.Repository
	manifestStore   *manifestStore
}

func (p *puller) pull(ctx context.Context, ref reference.Named) (err error) {
	// TODO(tiborvass): was ReceiveTimeout
	p.repo, err = newRepository(ctx, p.repoInfo, p.endpoint, p.config.MetaHeaders, p.config.AuthConfig, "pull")
	if err != nil {
		logrus.Warnf("Error getting v2 registry: %v", err)
		return err
	}

	p.manifestStore.remote, err = p.repo.Manifests(ctx)
	if err != nil {
		return err
	}

	if err = p.pullRepository(ctx, ref); err != nil {
		if _, ok := err.(fallbackError); ok {
			return err
		}
		if continueOnError(err, p.endpoint.Mirror) {
			return fallbackError{
				err:         err,
				transportOK: true,
			}
		}
	}
	return err
}

func (p *puller) pullRepository(ctx context.Context, ref reference.Named) (err error) {
	var layersDownloaded bool
	if !reference.IsNameOnly(ref) {
		layersDownloaded, err = p.pullTag(ctx, ref, p.config.Platform)
		if err != nil {
			return err
		}
	} else {
		tags, err := p.repo.Tags(ctx).All(ctx)
		if err != nil {
			return err
		}

		for _, tag := range tags {
			tagRef, err := reference.WithTag(ref, tag)
			if err != nil {
				return err
			}
			pulledNew, err := p.pullTag(ctx, tagRef, p.config.Platform)
			if err != nil {
				// Since this is the pull-all-tags case, don't
				// allow an error pulling a particular tag to
				// make the whole pull fall back to v1.
				if fallbackErr, ok := err.(fallbackError); ok {
					return fallbackErr.err
				}
				return err
			}
			// pulledNew is true if either new layers were downloaded OR if existing images were newly tagged
			// TODO(tiborvass): should we change the name of `layersDownload`? What about message in WriteStatus?
			layersDownloaded = layersDownloaded || pulledNew
		}
	}

	p.writeStatus(reference.FamiliarString(ref), layersDownloaded)

	return nil
}

// writeStatus writes a status message to out. If layersDownloaded is true, the
// status message indicates that a newer image was downloaded. Otherwise, it
// indicates that the image is up to date. requestedTag is the tag the message
// will refer to.
func (p *puller) writeStatus(requestedTag string, layersDownloaded bool) {
	if layersDownloaded {
		progress.Message(p.config.ProgressOutput, "", "Status: Downloaded newer image for "+requestedTag)
	} else {
		progress.Message(p.config.ProgressOutput, "", "Status: Image is up to date for "+requestedTag)
	}
}

type layerDescriptor struct {
	digest          digest.Digest
	diffID          layer.DiffID
	repoInfo        *registry.RepositoryInfo
	repo            distribution.Repository
	metadataService metadata.V2MetadataService
	tmpFile         *os.File
	verifier        digest.Verifier
	src             distribution.Descriptor
}

func (ld *layerDescriptor) Key() string {
	return "v2:" + ld.digest.String()
}

func (ld *layerDescriptor) ID() string {
	return stringid.TruncateID(ld.digest.String())
}

func (ld *layerDescriptor) DiffID() (layer.DiffID, error) {
	if ld.diffID != "" {
		return ld.diffID, nil
	}
	return ld.metadataService.GetDiffID(ld.digest)
}

func (ld *layerDescriptor) Download(ctx context.Context, progressOutput progress.Output) (io.ReadCloser, int64, error) {
	logrus.Debugf("pulling blob %q", ld.digest)

	var (
		err    error
		offset int64
	)

	if ld.tmpFile == nil {
		ld.tmpFile, err = createDownloadFile()
		if err != nil {
			return nil, 0, xfer.DoNotRetry{Err: err}
		}
	} else {
		offset, err = ld.tmpFile.Seek(0, io.SeekEnd)
		if err != nil {
			logrus.Debugf("error seeking to end of download file: %v", err)
			offset = 0

			ld.tmpFile.Close()
			if err := os.Remove(ld.tmpFile.Name()); err != nil {
				logrus.Errorf("Failed to remove temp file: %s", ld.tmpFile.Name())
			}
			ld.tmpFile, err = createDownloadFile()
			if err != nil {
				return nil, 0, xfer.DoNotRetry{Err: err}
			}
		} else if offset != 0 {
			logrus.Debugf("attempting to resume download of %q from %d bytes", ld.digest, offset)
		}
	}

	tmpFile := ld.tmpFile

	layerDownload, err := ld.open(ctx)
	if err != nil {
		logrus.Errorf("Error initiating layer download: %v", err)
		return nil, 0, retryOnError(err)
	}

	if offset != 0 {
		_, err := layerDownload.Seek(offset, io.SeekStart)
		if err != nil {
			if err := ld.truncateDownloadFile(); err != nil {
				return nil, 0, xfer.DoNotRetry{Err: err}
			}
			return nil, 0, err
		}
	}
	size, err := layerDownload.Seek(0, io.SeekEnd)
	if err != nil {
		// Seek failed, perhaps because there was no Content-Length
		// header. This shouldn't fail the download, because we can
		// still continue without a progress bar.
		size = 0
	} else {
		if size != 0 && offset > size {
			logrus.Debug("Partial download is larger than full blob. Starting over")
			offset = 0
			if err := ld.truncateDownloadFile(); err != nil {
				return nil, 0, xfer.DoNotRetry{Err: err}
			}
		}

		// Restore the seek offset either at the beginning of the
		// stream, or just after the last byte we have from previous
		// attempts.
		_, err = layerDownload.Seek(offset, io.SeekStart)
		if err != nil {
			return nil, 0, err
		}
	}

	reader := progress.NewProgressReader(ioutils.NewCancelReadCloser(ctx, layerDownload), progressOutput, size-offset, ld.ID(), "Downloading")
	defer reader.Close()

	if ld.verifier == nil {
		ld.verifier = ld.digest.Verifier()
	}

	_, err = io.Copy(tmpFile, io.TeeReader(reader, ld.verifier))
	if err != nil {
		if err == transport.ErrWrongCodeForByteRange {
			if err := ld.truncateDownloadFile(); err != nil {
				return nil, 0, xfer.DoNotRetry{Err: err}
			}
			return nil, 0, err
		}
		return nil, 0, retryOnError(err)
	}

	progress.Update(progressOutput, ld.ID(), "Verifying Checksum")

	if !ld.verifier.Verified() {
		err = fmt.Errorf("filesystem layer verification failed for digest %s", ld.digest)
		logrus.Error(err)

		// Allow a retry if this digest verification error happened
		// after a resumed download.
		if offset != 0 {
			if err := ld.truncateDownloadFile(); err != nil {
				return nil, 0, xfer.DoNotRetry{Err: err}
			}

			return nil, 0, err
		}
		return nil, 0, xfer.DoNotRetry{Err: err}
	}

	progress.Update(progressOutput, ld.ID(), "Download complete")

	logrus.Debugf("Downloaded %s to tempfile %s", ld.ID(), tmpFile.Name())

	_, err = tmpFile.Seek(0, io.SeekStart)
	if err != nil {
		tmpFile.Close()
		if err := os.Remove(tmpFile.Name()); err != nil {
			logrus.Errorf("Failed to remove temp file: %s", tmpFile.Name())
		}
		ld.tmpFile = nil
		ld.verifier = nil
		return nil, 0, xfer.DoNotRetry{Err: err}
	}

	// hand off the temporary file to the download manager, so it will only
	// be closed once
	ld.tmpFile = nil

	return ioutils.NewReadCloserWrapper(tmpFile, func() error {
		tmpFile.Close()
		err := os.RemoveAll(tmpFile.Name())
		if err != nil {
			logrus.Errorf("Failed to remove temp file: %s", tmpFile.Name())
		}
		return err
	}), size, nil
}

func (ld *layerDescriptor) Close() {
	if ld.tmpFile != nil {
		ld.tmpFile.Close()
		if err := os.RemoveAll(ld.tmpFile.Name()); err != nil {
			logrus.Errorf("Failed to remove temp file: %s", ld.tmpFile.Name())
		}
	}
}

func (ld *layerDescriptor) truncateDownloadFile() error {
	// Need a new hash context since we will be redoing the download
	ld.verifier = nil

	if _, err := ld.tmpFile.Seek(0, io.SeekStart); err != nil {
		logrus.Errorf("error seeking to beginning of download file: %v", err)
		return err
	}

	if err := ld.tmpFile.Truncate(0); err != nil {
		logrus.Errorf("error truncating download file: %v", err)
		return err
	}

	return nil
}

func (ld *layerDescriptor) Registered(diffID layer.DiffID) {
	// Cache mapping from this layer's DiffID to the blobsum
	_ = ld.metadataService.Add(diffID, metadata.V2Metadata{Digest: ld.digest, SourceRepository: ld.repoInfo.Name.Name()})
}

func (p *puller) pullTag(ctx context.Context, ref reference.Named, platform *specs.Platform) (tagUpdated bool, err error) {
	var (
		tagOrDigest string // Used for logging/progress only
		dgst        digest.Digest
		mt          string
		size        int64
		tagged      reference.NamedTagged
		isTagged    bool
	)
	if digested, isDigested := ref.(reference.Canonical); isDigested {
		dgst = digested.Digest()
		tagOrDigest = digested.String()
	} else if tagged, isTagged = ref.(reference.NamedTagged); isTagged {
		tagService := p.repo.Tags(ctx)
		desc, err := tagService.Get(ctx, tagged.Tag())
		if err != nil {
			return false, err
		}

		dgst = desc.Digest
		tagOrDigest = tagged.Tag()
		mt = desc.MediaType
		size = desc.Size
	} else {
		return false, fmt.Errorf("internal error: reference has neither a tag nor a digest: %s", reference.FamiliarString(ref))
	}

	ctx = log.WithLogger(ctx, logrus.WithFields(
		logrus.Fields{
			"digest": dgst,
			"remote": ref,
		}))

	desc := specs.Descriptor{
		MediaType: mt,
		Digest:    dgst,
		Size:      size,
	}
	manifest, err := p.manifestStore.Get(ctx, desc)
	if err != nil {
		if isTagged && isNotFound(errors.Cause(err)) {
			logrus.WithField("ref", ref).WithError(err).Debug("Falling back to pull manifest by tag")

			msg := `%s Failed to pull manifest by the resolved digest. This registry does not
	appear to conform to the distribution registry specification; falling back to
	pull by tag.  This fallback is DEPRECATED, and will be removed in a future
	release.  Please contact admins of %s. %s
`

			warnEmoji := "\U000026A0\U0000FE0F"
			progress.Messagef(p.config.ProgressOutput, "WARNING", msg, warnEmoji, p.endpoint.URL, warnEmoji)

			// Fetch by tag worked, but fetch by digest didn't.
			// This is a broken registry implementation.
			// We'll fallback to the old behavior and get the manifest by tag.
			var ms distribution.ManifestService
			ms, err = p.repo.Manifests(ctx)
			if err != nil {
				return false, err
			}

			manifest, err = ms.Get(ctx, "", distribution.WithTag(tagged.Tag()))
			err = errors.Wrap(err, "error after falling back to get manifest by tag")
		}
		if err != nil {
			return false, err
		}
	}

	if manifest == nil {
		return false, fmt.Errorf("image manifest does not exist for tag or digest %q", tagOrDigest)
	}

	if m, ok := manifest.(*schema2.DeserializedManifest); ok {
		if err := p.validateMediaType(m.Manifest.Config.MediaType); err != nil {
			return false, err
		}
	}

	logrus.Debugf("Pulling ref from V2 registry: %s", reference.FamiliarString(ref))
	progress.Message(p.config.ProgressOutput, tagOrDigest, "Pulling from "+reference.FamiliarName(p.repo.Named()))

	var (
		id             digest.Digest
		manifestDigest digest.Digest
	)

	switch v := manifest.(type) {
	case *schema1.SignedManifest:
		if p.config.RequireSchema2 {
			return false, fmt.Errorf("invalid manifest: not schema2")
		}

		// give registries time to upgrade to schema2 and only warn if we know a registry has been upgraded long time ago
		// TODO: condition to be removed
		if reference.Domain(ref) == "docker.io" {
			msg := fmt.Sprintf("Image %s uses outdated schema1 manifest format. Please upgrade to a schema2 image for better future compatibility. More information at https://docs.docker.com/registry/spec/deprecated-schema-v1/", ref)
			logrus.Warn(msg)
			progress.Message(p.config.ProgressOutput, "", msg)
		}

		id, manifestDigest, err = p.pullSchema1(ctx, ref, v, platform)
		if err != nil {
			return false, err
		}
	case *schema2.DeserializedManifest:
		id, manifestDigest, err = p.pullSchema2(ctx, ref, v, platform)
		if err != nil {
			return false, err
		}
	case *ocischema.DeserializedManifest:
		id, manifestDigest, err = p.pullOCI(ctx, ref, v, platform)
		if err != nil {
			return false, err
		}
	case *manifestlist.DeserializedManifestList:
		id, manifestDigest, err = p.pullManifestList(ctx, ref, v, platform)
		if err != nil {
			return false, err
		}
	default:
		return false, invalidManifestFormatError{}
	}

	progress.Message(p.config.ProgressOutput, "", "Digest: "+manifestDigest.String())

	if p.config.ReferenceStore != nil {
		oldTagID, err := p.config.ReferenceStore.Get(ref)
		if err == nil {
			if oldTagID == id {
				return false, addDigestReference(p.config.ReferenceStore, ref, manifestDigest, id)
			}
		} else if err != refstore.ErrDoesNotExist {
			return false, err
		}

		if canonical, ok := ref.(reference.Canonical); ok {
			if err = p.config.ReferenceStore.AddDigest(canonical, id, true); err != nil {
				return false, err
			}
		} else {
			if err = addDigestReference(p.config.ReferenceStore, ref, manifestDigest, id); err != nil {
				return false, err
			}
			if err = p.config.ReferenceStore.AddTag(ref, id, true); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// validateMediaType validates if the given mediaType is accepted by the puller's
// configuration.
func (p *puller) validateMediaType(mediaType string) error {
	var allowedMediaTypes []string
	if len(p.config.Schema2Types) > 0 {
		allowedMediaTypes = p.config.Schema2Types
	} else {
		allowedMediaTypes = defaultImageTypes
	}
	for _, t := range allowedMediaTypes {
		if mediaType == t {
			return nil
		}
	}

	configClass := mediaTypeClasses[mediaType]
	if configClass == "" {
		configClass = "unknown"
	}
	return invalidManifestClassError{mediaType, configClass}
}

func (p *puller) pullSchema1(ctx context.Context, ref reference.Reference, unverifiedManifest *schema1.SignedManifest, platform *specs.Platform) (id digest.Digest, manifestDigest digest.Digest, err error) {
	if platform != nil {
		// Early bath if the requested OS doesn't match that of the configuration.
		// This avoids doing the download, only to potentially fail later.
		if !system.IsOSSupported(platform.OS) {
			return "", "", fmt.Errorf("cannot download image with operating system %q when requesting %q", runtime.GOOS, platform.OS)
		}
	}

	var verifiedManifest *schema1.Manifest
	verifiedManifest, err = verifySchema1Manifest(unverifiedManifest, ref)
	if err != nil {
		return "", "", err
	}

	rootFS := image.NewRootFS()

	// remove duplicate layers and check parent chain validity
	err = fixManifestLayers(verifiedManifest)
	if err != nil {
		return "", "", err
	}

	var descriptors []xfer.DownloadDescriptor

	// Image history converted to the new format
	var history []image.History

	// Note that the order of this loop is in the direction of bottom-most
	// to top-most, so that the downloads slice gets ordered correctly.
	for i := len(verifiedManifest.FSLayers) - 1; i >= 0; i-- {
		blobSum := verifiedManifest.FSLayers[i].BlobSum
		if err = blobSum.Validate(); err != nil {
			return "", "", errors.Wrapf(err, "could not validate layer digest %q", blobSum)
		}

		var throwAway struct {
			ThrowAway bool `json:"throwaway,omitempty"`
		}
		if err := json.Unmarshal([]byte(verifiedManifest.History[i].V1Compatibility), &throwAway); err != nil {
			return "", "", err
		}

		h, err := v1.HistoryFromConfig([]byte(verifiedManifest.History[i].V1Compatibility), throwAway.ThrowAway)
		if err != nil {
			return "", "", err
		}
		history = append(history, h)

		if throwAway.ThrowAway {
			continue
		}

		layerDescriptor := &layerDescriptor{
			digest:          blobSum,
			repoInfo:        p.repoInfo,
			repo:            p.repo,
			metadataService: p.metadataService,
		}

		descriptors = append(descriptors, layerDescriptor)
	}

	resultRootFS, release, err := p.config.DownloadManager.Download(ctx, *rootFS, descriptors, p.config.ProgressOutput)
	if err != nil {
		return "", "", err
	}
	defer release()

	config, err := v1.MakeConfigFromV1Config([]byte(verifiedManifest.History[0].V1Compatibility), &resultRootFS, history)
	if err != nil {
		return "", "", err
	}

	imageID, err := p.config.ImageStore.Put(ctx, config)
	if err != nil {
		return "", "", err
	}

	manifestDigest = digest.FromBytes(unverifiedManifest.Canonical)

	return imageID, manifestDigest, nil
}

func (p *puller) pullSchema2Layers(ctx context.Context, target distribution.Descriptor, layers []distribution.Descriptor, platform *specs.Platform) (id digest.Digest, err error) {
	if _, err := p.config.ImageStore.Get(ctx, target.Digest); err == nil {
		// If the image already exists locally, no need to pull
		// anything.
		return target.Digest, nil
	}

	var descriptors []xfer.DownloadDescriptor

	// Note that the order of this loop is in the direction of bottom-most
	// to top-most, so that the downloads slice gets ordered correctly.
	for _, d := range layers {
		if err := d.Digest.Validate(); err != nil {
			return "", errors.Wrapf(err, "could not validate layer digest %q", d.Digest)
		}
		layerDescriptor := &layerDescriptor{
			digest:          d.Digest,
			repo:            p.repo,
			repoInfo:        p.repoInfo,
			metadataService: p.metadataService,
			src:             d,
		}

		descriptors = append(descriptors, layerDescriptor)
	}

	configChan := make(chan []byte, 1)
	configErrChan := make(chan error, 1)
	layerErrChan := make(chan error, 1)
	downloadsDone := make(chan struct{})
	var cancel func()
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()

	// Pull the image config
	go func() {
		configJSON, err := p.pullSchema2Config(ctx, target.Digest)
		if err != nil {
			configErrChan <- imageConfigPullError{Err: err}
			cancel()
			return
		}
		configChan <- configJSON
	}()

	var (
		configJSON       []byte          // raw serialized image config
		downloadedRootFS *image.RootFS   // rootFS from registered layers
		configRootFS     *image.RootFS   // rootFS from configuration
		release          func()          // release resources from rootFS download
		configPlatform   *specs.Platform // for LCOW when registering downloaded layers
	)

	layerStoreOS := runtime.GOOS
	if platform != nil {
		layerStoreOS = platform.OS
	}

	// https://github.com/docker/docker/issues/24766 - Err on the side of caution,
	// explicitly blocking images intended for linux from the Windows daemon. On
	// Windows, we do this before the attempt to download, effectively serialising
	// the download slightly slowing it down. We have to do it this way, as
	// chances are the download of layers itself would fail due to file names
	// which aren't suitable for NTFS. At some point in the future, if a similar
	// check to block Windows images being pulled on Linux is implemented, it
	// may be necessary to perform the same type of serialisation.
	if runtime.GOOS == "windows" {
		configJSON, configRootFS, configPlatform, err = receiveConfig(configChan, configErrChan)
		if err != nil {
			return "", err
		}
		if configRootFS == nil {
			return "", errRootFSInvalid
		}
		if err := checkImageCompatibility(configPlatform.OS, configPlatform.OSVersion); err != nil {
			return "", err
		}

		if len(descriptors) != len(configRootFS.DiffIDs) {
			return "", errRootFSMismatch
		}
		if platform == nil {
			// Early bath if the requested OS doesn't match that of the configuration.
			// This avoids doing the download, only to potentially fail later.
			if !system.IsOSSupported(configPlatform.OS) {
				return "", fmt.Errorf("cannot download image with operating system %q when requesting %q", configPlatform.OS, layerStoreOS)
			}
			layerStoreOS = configPlatform.OS
		}

		// Populate diff ids in descriptors to avoid downloading foreign layers
		// which have been side loaded
		for i := range descriptors {
			descriptors[i].(*layerDescriptor).diffID = configRootFS.DiffIDs[i]
		}
	}

	// Assume that the operating system is the host OS if blank, and validate it
	// to ensure we don't cause a panic by an invalid index into the layerstores.
	if layerStoreOS != "" && !system.IsOSSupported(layerStoreOS) {
		return "", system.ErrNotSupportedOperatingSystem
	}

	if p.config.DownloadManager != nil {
		go func() {
			var (
				err    error
				rootFS image.RootFS
			)
			downloadRootFS := *image.NewRootFS()
			rootFS, release, err = p.config.DownloadManager.Download(ctx, downloadRootFS, descriptors, p.config.ProgressOutput)
			if err != nil {
				// Intentionally do not cancel the config download here
				// as the error from config download (if there is one)
				// is more interesting than the layer download error
				layerErrChan <- err
				return
			}

			downloadedRootFS = &rootFS
			close(downloadsDone)
		}()
	} else {
		// We have nothing to download
		close(downloadsDone)
	}

	if configJSON == nil {
		configJSON, configRootFS, _, err = receiveConfig(configChan, configErrChan)
		if err == nil && configRootFS == nil {
			err = errRootFSInvalid
		}
		if err != nil {
			cancel()
			select {
			case <-downloadsDone:
			case <-layerErrChan:
			}
			return "", err
		}
	}

	select {
	case <-downloadsDone:
	case err = <-layerErrChan:
		return "", err
	}

	if release != nil {
		defer release()
	}

	if downloadedRootFS != nil {
		// The DiffIDs returned in rootFS MUST match those in the config.
		// Otherwise the image config could be referencing layers that aren't
		// included in the manifest.
		if len(downloadedRootFS.DiffIDs) != len(configRootFS.DiffIDs) {
			return "", errRootFSMismatch
		}

		for i := range downloadedRootFS.DiffIDs {
			if downloadedRootFS.DiffIDs[i] != configRootFS.DiffIDs[i] {
				return "", errRootFSMismatch
			}
		}
	}

	imageID, err := p.config.ImageStore.Put(ctx, configJSON)
	if err != nil {
		return "", err
	}

	return imageID, nil
}

func (p *puller) pullSchema2(ctx context.Context, ref reference.Named, mfst *schema2.DeserializedManifest, platform *specs.Platform) (id digest.Digest, manifestDigest digest.Digest, err error) {
	manifestDigest, err = schema2ManifestDigest(ref, mfst)
	if err != nil {
		return "", "", err
	}
	id, err = p.pullSchema2Layers(ctx, mfst.Target(), mfst.Layers, platform)
	return id, manifestDigest, err
}

func (p *puller) pullOCI(ctx context.Context, ref reference.Named, mfst *ocischema.DeserializedManifest, platform *specs.Platform) (id digest.Digest, manifestDigest digest.Digest, err error) {
	manifestDigest, err = schema2ManifestDigest(ref, mfst)
	if err != nil {
		return "", "", err
	}
	id, err = p.pullSchema2Layers(ctx, mfst.Target(), mfst.Layers, platform)
	return id, manifestDigest, err
}

func receiveConfig(configChan <-chan []byte, errChan <-chan error) ([]byte, *image.RootFS, *specs.Platform, error) {
	select {
	case configJSON := <-configChan:
		rootfs, err := rootFSFromConfig(configJSON)
		if err != nil {
			return nil, nil, nil, err
		}
		platform, err := platformFromConfig(configJSON)
		if err != nil {
			return nil, nil, nil, err
		}
		return configJSON, rootfs, platform, nil
	case err := <-errChan:
		return nil, nil, nil, err
		// Don't need a case for ctx.Done in the select because cancellation
		// will trigger an error in p.pullSchema2ImageConfig.
	}
}

// pullManifestList handles "manifest lists" which point to various
// platform-specific manifests.
func (p *puller) pullManifestList(ctx context.Context, ref reference.Named, mfstList *manifestlist.DeserializedManifestList, pp *specs.Platform) (id digest.Digest, manifestListDigest digest.Digest, err error) {
	manifestListDigest, err = schema2ManifestDigest(ref, mfstList)
	if err != nil {
		return "", "", err
	}

	var platform specs.Platform
	if pp != nil {
		platform = *pp
	}
	logrus.Debugf("%s resolved to a manifestList object with %d entries; looking for a %s/%s match", ref, len(mfstList.Manifests), platforms.Format(platform), runtime.GOARCH)

	manifestMatches := filterManifests(mfstList.Manifests, platform)

	for _, match := range manifestMatches {
		if err := checkImageCompatibility(match.Platform.OS, match.Platform.OSVersion); err != nil {
			return "", "", err
		}

		desc := specs.Descriptor{
			Digest:    match.Digest,
			Size:      match.Size,
			MediaType: match.MediaType,
		}
		manifest, err := p.manifestStore.Get(ctx, desc)
		if err != nil {
			return "", "", err
		}

		manifestRef, err := reference.WithDigest(reference.TrimNamed(ref), match.Digest)
		if err != nil {
			return "", "", err
		}

		switch v := manifest.(type) {
		case *schema1.SignedManifest:
			msg := fmt.Sprintf("[DEPRECATION NOTICE] v2 schema1 manifests in manifest lists are not supported and will break in a future release. Suggest author of %s to upgrade to v2 schema2. More information at https://docs.docker.com/registry/spec/deprecated-schema-v1/", ref)
			logrus.Warn(msg)
			progress.Message(p.config.ProgressOutput, "", msg)

			platform := toOCIPlatform(match.Platform)
			id, _, err = p.pullSchema1(ctx, manifestRef, v, platform)
			if err != nil {
				return "", "", err
			}
		case *schema2.DeserializedManifest:
			platform := toOCIPlatform(match.Platform)
			id, _, err = p.pullSchema2(ctx, manifestRef, v, platform)
			if err != nil {
				return "", "", err
			}
		case *ocischema.DeserializedManifest:
			platform := toOCIPlatform(match.Platform)
			id, _, err = p.pullOCI(ctx, manifestRef, v, platform)
			if err != nil {
				return "", "", err
			}
		case *manifestlist.DeserializedManifestList:
			id, _, err = p.pullManifestList(ctx, manifestRef, v, pp)
			if err != nil {
				var noMatches noMatchesErr
				if !errors.As(err, &noMatches) {
					// test the next match
					continue
				}
			}
		default:
			// OCI spec requires to skip unknown manifest types
			continue
		}
		return id, manifestListDigest, err
	}
	return "", "", noMatchesErr{platform: platform}
}

const (
	defaultSchemaPullBackoff     = 250 * time.Millisecond
	defaultMaxSchemaPullAttempts = 5
)

func (p *puller) pullSchema2Config(ctx context.Context, dgst digest.Digest) (configJSON []byte, err error) {
	blobs := p.repo.Blobs(ctx)
	err = retry(ctx, defaultMaxSchemaPullAttempts, defaultSchemaPullBackoff, func(ctx context.Context) (err error) {
		configJSON, err = blobs.Get(ctx, dgst)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Verify image config digest
	verifier := dgst.Verifier()
	if _, err := verifier.Write(configJSON); err != nil {
		return nil, err
	}
	if !verifier.Verified() {
		err := fmt.Errorf("image config verification failed for digest %s", dgst)
		logrus.Error(err)
		return nil, err
	}

	return configJSON, nil
}

type noMatchesErr struct {
	platform specs.Platform
}

func (e noMatchesErr) Error() string {
	return fmt.Sprintf("no matching manifest for %s in the manifest list entries", formatPlatform(e.platform))
}

func retry(ctx context.Context, maxAttempts int, sleep time.Duration, f func(ctx context.Context) error) (err error) {
	attempt := 0
	for ; attempt < maxAttempts; attempt++ {
		err = retryOnError(f(ctx))
		if err == nil {
			return nil
		}
		if xfer.IsDoNotRetryError(err) {
			break
		}

		if attempt+1 < maxAttempts {
			timer := time.NewTimer(sleep)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				logrus.WithError(err).WithField("attempts", attempt+1).Debug("retrying after error")
				sleep *= 2
			}
		}
	}
	return errors.Wrapf(err, "download failed after attempts=%d", attempt+1)
}

// schema2ManifestDigest computes the manifest digest, and, if pulling by
// digest, ensures that it matches the requested digest.
func schema2ManifestDigest(ref reference.Named, mfst distribution.Manifest) (digest.Digest, error) {
	_, canonical, err := mfst.Payload()
	if err != nil {
		return "", err
	}

	// If pull by digest, then verify the manifest digest.
	if digested, isDigested := ref.(reference.Canonical); isDigested {
		verifier := digested.Digest().Verifier()
		if _, err := verifier.Write(canonical); err != nil {
			return "", err
		}
		if !verifier.Verified() {
			err := fmt.Errorf("manifest verification failed for digest %s", digested.Digest())
			logrus.Error(err)
			return "", err
		}
		return digested.Digest(), nil
	}

	return digest.FromBytes(canonical), nil
}

func verifySchema1Manifest(signedManifest *schema1.SignedManifest, ref reference.Reference) (m *schema1.Manifest, err error) {
	// If pull by digest, then verify the manifest digest. NOTE: It is
	// important to do this first, before any other content validation. If the
	// digest cannot be verified, don't even bother with those other things.
	if digested, isCanonical := ref.(reference.Canonical); isCanonical {
		verifier := digested.Digest().Verifier()
		if _, err := verifier.Write(signedManifest.Canonical); err != nil {
			return nil, err
		}
		if !verifier.Verified() {
			err := fmt.Errorf("image verification failed for digest %s", digested.Digest())
			logrus.Error(err)
			return nil, err
		}
	}
	m = &signedManifest.Manifest

	if m.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported schema version %d for %q", m.SchemaVersion, reference.FamiliarString(ref))
	}
	if len(m.FSLayers) != len(m.History) {
		return nil, fmt.Errorf("length of history not equal to number of layers for %q", reference.FamiliarString(ref))
	}
	if len(m.FSLayers) == 0 {
		return nil, fmt.Errorf("no FSLayers in manifest for %q", reference.FamiliarString(ref))
	}
	return m, nil
}

// fixManifestLayers removes repeated layers from the manifest and checks the
// correctness of the parent chain.
func fixManifestLayers(m *schema1.Manifest) error {
	imgs := make([]*image.V1Image, len(m.FSLayers))
	for i := range m.FSLayers {
		img := &image.V1Image{}

		if err := json.Unmarshal([]byte(m.History[i].V1Compatibility), img); err != nil {
			return err
		}

		imgs[i] = img
		if err := v1.ValidateID(img.ID); err != nil {
			return err
		}
	}

	if imgs[len(imgs)-1].Parent != "" && runtime.GOOS != "windows" {
		// Windows base layer can point to a base layer parent that is not in manifest.
		return errors.New("invalid parent ID in the base layer of the image")
	}

	// check general duplicates to error instead of a deadlock
	idmap := make(map[string]struct{})

	var lastID string
	for _, img := range imgs {
		// skip IDs that appear after each other, we handle those later
		if _, exists := idmap[img.ID]; img.ID != lastID && exists {
			return fmt.Errorf("ID %+v appears multiple times in manifest", img.ID)
		}
		lastID = img.ID
		idmap[lastID] = struct{}{}
	}

	// backwards loop so that we keep the remaining indexes after removing items
	for i := len(imgs) - 2; i >= 0; i-- {
		if imgs[i].ID == imgs[i+1].ID { // repeated ID. remove and continue
			m.FSLayers = append(m.FSLayers[:i], m.FSLayers[i+1:]...)
			m.History = append(m.History[:i], m.History[i+1:]...)
		} else if imgs[i].Parent != imgs[i+1].ID {
			return fmt.Errorf("invalid parent ID. Expected %v, got %v", imgs[i+1].ID, imgs[i].Parent)
		}
	}

	return nil
}

func createDownloadFile() (*os.File, error) {
	return os.CreateTemp("", "GetImageBlob")
}

func toOCIPlatform(p manifestlist.PlatformSpec) *specs.Platform {
	// distribution pkg does define platform as pointer so this hack for empty struct
	// is necessary. This is temporary until correct OCI image-spec package is used.
	if p.OS == "" && p.Architecture == "" && p.Variant == "" && p.OSVersion == "" && p.OSFeatures == nil && p.Features == nil {
		return nil
	}
	return &specs.Platform{
		OS:           p.OS,
		Architecture: p.Architecture,
		Variant:      p.Variant,
		OSFeatures:   p.OSFeatures,
		OSVersion:    p.OSVersion,
	}
}

// maximumSpec returns the distribution platform with maximum compatibility for the current node.
func maximumSpec() specs.Platform {
	p := platforms.DefaultSpec()
	if p.Architecture == "amd64" {
		p.Variant = archvariant.AMD64Variant()
	}
	return p
}
