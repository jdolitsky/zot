//go:build sync
// +build sync

package references

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	godigest "github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sigstore/cosign/v2/pkg/oci/remote"

	zerr "zotregistry.io/zot/errors"
	"zotregistry.io/zot/pkg/common"
	"zotregistry.io/zot/pkg/extensions/sync/constants"
	client "zotregistry.io/zot/pkg/extensions/sync/httpclient"
	"zotregistry.io/zot/pkg/log"
	"zotregistry.io/zot/pkg/meta/repodb"
	"zotregistry.io/zot/pkg/storage"
)

type CosignReference struct {
	client          *client.Client
	storeController storage.StoreController
	repoDB          repodb.RepoDB
	log             log.Logger
}

func NewCosignReference(httpClient *client.Client, storeController storage.StoreController,
	repoDB repodb.RepoDB, log log.Logger,
) CosignReference {
	return CosignReference{
		client:          httpClient,
		storeController: storeController,
		repoDB:          repoDB,
		log:             log,
	}
}

func (ref CosignReference) Name() string {
	return constants.Cosign
}

func (ref CosignReference) IsSigned(upstreamRepo, subjectDigestStr string) bool {
	cosignSignatureTag := getCosignSignatureTagFromSubjectDigest(subjectDigestStr)
	_, _, err := ref.getManifest(upstreamRepo, cosignSignatureTag)

	return err == nil
}

func (ref CosignReference) canSkipReferences(localRepo, digest string, manifest *ispec.Manifest) (
	bool, error,
) {
	if manifest == nil {
		return true, nil
	}

	imageStore := ref.storeController.GetImageStore(localRepo)

	// check cosign signature already synced
	_, localDigest, _, err := imageStore.GetImageManifest(localRepo, digest)
	if err != nil {
		if errors.Is(err, zerr.ErrManifestNotFound) {
			return false, nil
		}

		ref.log.Error().Str("errorType", common.TypeOf(err)).Err(err).
			Str("repository", localRepo).Str("reference", digest).
			Msg("couldn't get local cosign manifest")

		return false, err
	}

	if localDigest.String() != digest {
		return false, nil
	}

	ref.log.Info().Str("repository", localRepo).Str("reference", digest).
		Msg("skipping syncing cosign reference, already synced")

	return true, nil
}

func (ref CosignReference) SyncReferences(localRepo, remoteRepo, subjectDigestStr string) ([]godigest.Digest, error) {
	cosignTags := getCosignTagsFromSubjectDigest(subjectDigestStr)

	refsDigests := make([]godigest.Digest, 0, len(cosignTags))

	for _, cosignTag := range cosignTags {
		manifest, manifestBuf, err := ref.getManifest(remoteRepo, cosignTag)
		if err != nil {
			if errors.Is(err, zerr.ErrSyncReferrerNotFound) {
				continue
			}

			return refsDigests, err
		}

		digest := godigest.FromBytes(manifestBuf)

		skip, err := ref.canSkipReferences(localRepo, digest.String(), manifest)
		if err != nil {
			ref.log.Error().Err(err).Str("repository", localRepo).Str("subject", subjectDigestStr).
				Msg("couldn't check if the remote image cosign reference can be skipped")
		}

		if skip {
			refsDigests = append(refsDigests, digest)

			continue
		}

		imageStore := ref.storeController.GetImageStore(localRepo)

		ref.log.Info().Str("repository", localRepo).Str("subject", subjectDigestStr).
			Msg("syncing cosign reference for image")

		for _, blob := range manifest.Layers {
			if err := syncBlob(ref.client, imageStore, localRepo, remoteRepo, blob.Digest, ref.log); err != nil {
				return refsDigests, err
			}
		}

		// sync config blob
		if err := syncBlob(ref.client, imageStore, localRepo, remoteRepo, manifest.Config.Digest, ref.log); err != nil {
			return refsDigests, err
		}

		// push manifest
		referenceDigest, _, err := imageStore.PutImageManifest(localRepo, cosignTag,
			ispec.MediaTypeImageManifest, manifestBuf)
		if err != nil {
			ref.log.Error().Str("errorType", common.TypeOf(err)).
				Str("repository", localRepo).Str("subject", subjectDigestStr).
				Err(err).Msg("couldn't upload cosign reference manifest for image")

			return refsDigests, err
		}

		refsDigests = append(refsDigests, digest)

		ref.log.Info().Str("repository", localRepo).Str("subject", subjectDigestStr).
			Msg("successfully synced cosign reference for image")

		if ref.repoDB != nil {
			ref.log.Debug().Str("repository", localRepo).Str("subject", subjectDigestStr).
				Msg("repoDB: trying to sync cosign reference for image")

			isSig, sigType, signedManifestDig, err := storage.CheckIsImageSignature(localRepo, manifestBuf,
				cosignTag)
			if err != nil {
				return refsDigests, fmt.Errorf("failed to check if cosign reference '%s@%s' is a signature: %w", localRepo,
					cosignTag, err)
			}

			if isSig {
				err = ref.repoDB.AddManifestSignature(localRepo, signedManifestDig, repodb.SignatureMetadata{
					SignatureType:   sigType,
					SignatureDigest: referenceDigest.String(),
				})
			} else {
				err = repodb.SetImageMetaFromInput(localRepo, cosignTag, ispec.MediaTypeImageManifest,
					referenceDigest, manifestBuf, ref.storeController.GetImageStore(localRepo),
					ref.repoDB, ref.log)
			}

			if err != nil {
				return refsDigests, fmt.Errorf("failed to set metadata for cosign reference in '%s@%s': %w",
					localRepo, subjectDigestStr, err)
			}

			ref.log.Info().Str("repository", localRepo).Str("subject", subjectDigestStr).
				Msg("repoDB: successfully added cosign reference for image")
		}
	}

	return refsDigests, nil
}

func (ref CosignReference) getManifest(repo, cosignTag string) (*ispec.Manifest, []byte, error) {
	var cosignManifest ispec.Manifest

	body, _, statusCode, err := ref.client.MakeGetRequest(&cosignManifest, ispec.MediaTypeImageManifest,
		"v2", repo, "manifests", cosignTag)
	if err != nil {
		if statusCode == http.StatusNotFound {
			ref.log.Debug().Str("errorType", common.TypeOf(err)).
				Str("repository", repo).Str("tag", cosignTag).
				Err(err).Msg("couldn't find any cosign manifest for image")

			return nil, nil, zerr.ErrSyncReferrerNotFound
		}

		ref.log.Error().Str("errorType", common.TypeOf(err)).
			Str("repository", repo).Str("tag", cosignTag).Int("statusCode", statusCode).
			Err(err).Msg("couldn't get cosign manifest for image")

		return nil, nil, err
	}

	return &cosignManifest, body, nil
}

func getCosignSignatureTagFromSubjectDigest(digestStr string) string {
	return strings.Replace(digestStr, ":", "-", 1) + "." + remote.SignatureTagSuffix
}

func getCosignSBOMTagFromSubjectDigest(digestStr string) string {
	return strings.Replace(digestStr, ":", "-", 1) + "." + remote.SBOMTagSuffix
}

func getCosignTagsFromSubjectDigest(digestStr string) []string {
	var cosignTags []string

	// signature tag
	cosignTags = append(cosignTags, getCosignSignatureTagFromSubjectDigest(digestStr))
	// sbom tag
	cosignTags = append(cosignTags, getCosignSBOMTagFromSubjectDigest(digestStr))

	return cosignTags
}

// this function will check if tag is a cosign tag (signature or sbom).
func IsCosignTag(tag string) bool {
	if strings.HasPrefix(tag, "sha256-") &&
		(strings.HasSuffix(tag, remote.SignatureTagSuffix) || strings.HasSuffix(tag, remote.SBOMTagSuffix)) {
		return true
	}

	return false
}
