// FIXME(thaJeztah): remove once we are a module; the go:build directive prevents go from downgrading language version to go1.16:
//go:build go1.22

package containerd

import (
	"context"
	"sync/atomic"
	"time"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/backend"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/storage"
	"github.com/docker/docker/internal/sliceutil"
	imagespec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/semaphore"
)

func (i *ImageService) ImageInspect(ctx context.Context, refOrID string, opts backend.ImageInspectOpts) (*imagetypes.InspectResponse, error) {
	c8dImg, err := i.resolveImage(ctx, refOrID)
	if err != nil {
		return nil, err
	}

	target := c8dImg.Target
	tagged, err := i.images.List(ctx, "target.digest=="+target.Digest.String())
	if err != nil {
		return nil, err
	}

	// This could happen only if the image was deleted after the resolveImage call above.
	if len(tagged) == 0 {
		return nil, errInconsistentData
	}

	lastUpdated := time.Unix(0, 0)
	for _, i := range tagged {
		if i.UpdatedAt.After(lastUpdated) {
			lastUpdated = i.UpdatedAt
		}
	}

	platform := matchAllWithPreference(platforms.Default())
	size, err := i.size(ctx, target, platform)
	if err != nil {
		return nil, err
	}

	multi, err := i.multiPlatformSummary(ctx, c8dImg, platform)
	if err != nil {
		return nil, err
	}

	if multi.Best == nil {
		return nil, &errPlatformNotFound{
			wanted:   platforms.DefaultSpec(),
			imageRef: refOrID,
		}
	}

	best := multi.Best
	var img imagespec.DockerOCIImage
	if err := best.ReadConfig(ctx, &img); err != nil {
		return nil, err
	}

	var comment string
	if len(comment) == 0 && len(img.History) > 0 {
		comment = img.History[len(img.History)-1].Comment
	}

	var created string
	if img.Created != nil {
		created = img.Created.Format(time.RFC3339Nano)
	}

	var layers []string
	for _, layer := range img.RootFS.DiffIDs {
		layers = append(layers, layer.String())
	}

	parent, err := i.getImageLabelByDigest(ctx, target.Digest, imageLabelClassicBuilderParent)
	if err != nil {
		log.G(ctx).WithError(err).Warn("failed to determine Parent property")
	}

	var manifests []imagetypes.ManifestSummary
	if opts.Manifests {
		manifests = multi.Manifests
	}

	repoTags, repoDigests := collectRepoTagsAndDigests(ctx, tagged)

	return &imagetypes.InspectResponse{
		ID:            target.Digest.String(),
		RepoTags:      repoTags,
		Descriptor:    &target,
		RepoDigests:   repoDigests,
		Parent:        parent,
		Comment:       comment,
		Created:       created,
		DockerVersion: "",
		Author:        img.Author,
		Config:        dockerOCIImageConfigToContainerConfig(img.Config),
		Architecture:  img.Architecture,
		Variant:       img.Variant,
		Os:            img.OS,
		OsVersion:     img.OSVersion,
		Size:          size,
		Manifests:     manifests,
		GraphDriver: storage.DriverData{
			Name: i.snapshotter,
			Data: nil,
		},
		RootFS: imagetypes.RootFS{
			Type:   img.RootFS.Type,
			Layers: layers,
		},
		Metadata: imagetypes.Metadata{
			LastTagTime: lastUpdated,
		},
	}, nil
}

func collectRepoTagsAndDigests(ctx context.Context, tagged []c8dimages.Image) (repoTags []string, repoDigests []string) {
	repoTags = make([]string, 0, len(tagged))
	repoDigests = make([]string, 0, len(tagged))
	for _, img := range tagged {
		if isDanglingImage(img) {
			if len(tagged) > 1 {
				// This is unexpected - dangling image should be deleted
				// as soon as another image with the same target is created.
				// Log a warning, but don't error out the whole operation.
				log.G(ctx).WithField("refs", tagged).Warn("multiple images have the same target, but one of them is still dangling")
			}
			continue
		}

		name, err := reference.ParseNamed(img.Name)
		if err != nil {
			log.G(ctx).WithField("name", name).WithError(err).Error("failed to parse image name as reference")
			// Include the malformed name in RepoTags to be consistent with `docker image ls`.
			repoTags = append(repoTags, img.Name)
			continue
		}

		repoTags = append(repoTags, reference.FamiliarString(name))
		if _, ok := name.(reference.Digested); ok {
			repoDigests = append(repoDigests, reference.FamiliarString(name))
			// Image name is a digested reference already, so no need to create a digested reference.
			continue
		}

		digested, err := reference.WithDigest(reference.TrimNamed(name), img.Target.Digest)
		if err != nil {
			// This could only happen if digest is invalid, but considering that
			// we get it from the Descriptor it's highly unlikely.
			// Log error just in case.
			log.G(ctx).WithError(err).Error("failed to create digested reference")
			continue
		}
		repoDigests = append(repoDigests, reference.FamiliarString(digested))
	}
	return sliceutil.Dedup(repoTags), sliceutil.Dedup(repoDigests)
}

// size returns the total size of the image's packed resources.
func (i *ImageService) size(ctx context.Context, desc ocispec.Descriptor, platform platforms.MatchComparer) (int64, error) {
	var size atomic.Int64

	cs := i.content
	handler := c8dimages.LimitManifests(c8dimages.ChildrenHandler(cs), platform, 1)

	var wh c8dimages.HandlerFunc = func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		children, err := handler(ctx, desc)
		if err != nil {
			if !cerrdefs.IsNotFound(err) {
				return nil, err
			}
		}

		size.Add(desc.Size)

		return children, nil
	}

	l := semaphore.NewWeighted(3)
	if err := c8dimages.Dispatch(ctx, wh, l, desc); err != nil {
		return 0, err
	}

	return size.Load(), nil
}
