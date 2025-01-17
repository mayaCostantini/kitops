// Copyright 2024 The KitOps Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"kitops/pkg/artifact"
	"kitops/pkg/lib/constants"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
)

const (
	DefaultRegistry   = "localhost"
	DefaultRepository = "_"
)

var (
	validTagRegex = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
)

// ParseReference parses a reference string into a Reference struct. It attempts to make
// references conform to an expected structure, with a defined registry and repository by filling
// default values for registry and repository where appropriate. Where the first part of a reference
// doesn't look like a registry URL, the default registry is used, turning e.g. testorg/testrepo into
// localhost/testorg/testrepo. If refString does not contain a registry or a repository (i.e. is a
// base SHA256 hash), the returned reference uses placeholder values for registry and repository.
//
// See FormatRepositoryForDisplay for removing default values from a registry for displaying to the
// user.
func ParseReference(refString string) (ref *registry.Reference, extraTags []string, err error) {
	// Check if provided input is a plain digest
	if _, err := digest.Parse(refString); err == nil {
		ref := &registry.Reference{
			Registry:   DefaultRegistry,
			Repository: DefaultRepository,
			Reference:  refString,
		}
		return ref, []string{}, nil
	}

	// Handle registry, which may or may not be specified; if unspecified, use a default value for registry
	refParts := strings.Split(refString, "/")
	if len(refParts) == 1 {
		// Just a repo, need to add default registry
		refString = fmt.Sprintf("%s/%s", DefaultRegistry, refString)
	} else {
		// Check if registry part "looks" like a URL; we're trying to distinguish between cases:
		// a) testorg/testrepo --> should be localhost/testorg/testrepo
		// b) registry.io/testrepo --> should be registry.io/testrepo
		// c) localhost:5000/testrepo --> should be localhost:5000/testrepo
		registry := refParts[0]
		if !strings.Contains(registry, ":") && !strings.Contains(registry, ".") {
			refString = fmt.Sprintf("%s/%s", DefaultRegistry, refString)
		}
	}

	// Split off extra tags (e.g. repo:tag1,tag2,tag3)
	refAndTags := strings.Split(refString, ",")
	baseRef, err := registry.ParseReference(refAndTags[0])
	if err != nil {
		return nil, nil, err
	}
	return &baseRef, refAndTags[1:], nil
}

// DefaultReference returns a reference that can be used when no reference is supplied. It uses
// the default registry and repository
func DefaultReference() *registry.Reference {
	return &registry.Reference{
		Registry:   DefaultRegistry,
		Repository: DefaultRepository,
	}
}

// FormatRepositoryForDisplay removes default values from a repository string to avoid surfacing defaulted fields
// when displaying references, which may be confusing.
func FormatRepositoryForDisplay(repo string) string {
	repo = strings.TrimPrefix(repo, DefaultRegistry+"/")
	repo = strings.TrimPrefix(repo, DefaultRepository)
	return repo
}

// RepoPath returns the path that should be used for creating a local OCI index given a
// specific *registry.Reference.
func RepoPath(storagePath string, ref *registry.Reference) string {
	return filepath.Join(storagePath, ref.Registry, ref.Repository)
}

// GetManifestAndConfig returns the manifest and config (Kitfile) for a manifest Descriptor.
// Calls GetManifest and GetConfig.
func GetManifestAndConfig(ctx context.Context, store content.Storage, manifestDesc ocispec.Descriptor) (*ocispec.Manifest, *artifact.KitFile, error) {
	manifest, err := GetManifest(ctx, store, manifestDesc)
	if err != nil {
		return nil, nil, err
	}
	config, err := GetConfig(ctx, store, manifest.Config)
	if err != nil {
		return nil, nil, err
	}
	return manifest, config, nil
}

// GetManifest returns the Manifest described by a Descriptor. Returns an error if the manifest blob cannot be
// resolved or does not represent a modelkit manifest.
func GetManifest(ctx context.Context, store content.Storage, manifestDesc ocispec.Descriptor) (*ocispec.Manifest, error) {
	manifestBytes, err := content.FetchAll(ctx, store, manifestDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest %s: %w", manifestDesc.Digest, err)
	}
	manifest := &ocispec.Manifest{}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse manifest %s: %w", manifestDesc.Digest, err)
	}
	if manifest.Config.MediaType != constants.ModelConfigMediaType {
		return nil, fmt.Errorf("reference exists but is not a model")
	}

	return manifest, nil
}

// GetConfig returns the config (Kitfile) described by a descriptor. Returns an error if the config blob cannot
// be resolved or if the descriptor does not describe a Kitfile.
func GetConfig(ctx context.Context, store content.Storage, configDesc ocispec.Descriptor) (*artifact.KitFile, error) {
	if configDesc.MediaType != constants.ModelConfigMediaType {
		return nil, fmt.Errorf("configuration descriptor does not describe a Kitfile")
	}
	configBytes, err := content.FetchAll(ctx, store, configDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}
	config := &artifact.KitFile{}
	if err := json.Unmarshal(configBytes, config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	return config, nil
}

// ResolveManifest returns the manifest for a reference (tag), if present in the target store
func ResolveManifest(ctx context.Context, store oras.Target, reference string) (*ocispec.Manifest, error) {
	desc, err := store.Resolve(ctx, reference)
	if err != nil {
		return nil, fmt.Errorf("reference %s not found in remote repository: %w", reference, err)
	}
	return GetManifest(ctx, store, desc)
}

// ResolveManifestAndConfig returns the manifest and config (Kitfile) for a given reference (tag), if present
// in the store. Calls ResolveManifest and GetConfig.
func ResolveManifestAndConfig(ctx context.Context, store oras.Target, reference string) (*ocispec.Manifest, *artifact.KitFile, error) {
	manifest, err := ResolveManifest(ctx, store, reference)
	if err != nil {
		return nil, nil, err
	}
	config, err := GetConfig(ctx, store, manifest.Config)
	if err != nil {
		return nil, nil, err
	}
	return manifest, config, nil
}

// GetTagsForDescriptor returns the list of tags that reference a particular descriptor (SHA256 hash) in LocalStorage.
func GetTagsForDescriptor(ctx context.Context, store LocalStorage, desc ocispec.Descriptor) ([]string, error) {
	index, err := store.GetIndex()
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, manifest := range index.Manifests {
		if manifest.Digest == desc.Digest && manifest.Annotations[ocispec.AnnotationRefName] != "" {
			tags = append(tags, manifest.Annotations[ocispec.AnnotationRefName])
		}
	}
	return tags, nil
}

func ValidateTag(tag string) error {
	if !validTagRegex.MatchString(tag) {
		return fmt.Errorf("invalid tag")
	}
	return nil
}
