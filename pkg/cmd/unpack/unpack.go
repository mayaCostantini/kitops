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

package unpack

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"kitops/pkg/artifact"
	"kitops/pkg/lib/constants"
	"kitops/pkg/lib/filesystem"
	"kitops/pkg/lib/repo"
	"kitops/pkg/output"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
)

// unpackModel fetches and unpacks a *registry.Reference from an oras.Target. It returns an error if
// unpacking fails, or if any path specified in the modelkit is not a subdirectory of the current
// unpack target directory.
func unpackModel(ctx context.Context, store oras.Target, ref *registry.Reference, options *unpackOptions) error {
	manifestDesc, err := store.Resolve(ctx, ref.Reference)
	if err != nil {
		return fmt.Errorf("failed to resolve local reference: %w", err)
	}
	manifest, config, err := repo.GetManifestAndConfig(ctx, store, manifestDesc)
	if err != nil {
		return fmt.Errorf("failed to read local model: %s", err)
	}

	if options.unpackConf.unpackConfig {
		if err := unpackConfig(config, options.unpackDir, options.overwrite); err != nil {
			return err
		}
	}

	// Since there might be multiple models, etc. we need to synchronously iterate
	// through the config's relevant field to get the correct path for unpacking
	var codeIdx, datasetIdx int
	for _, layerDesc := range manifest.Layers {
		var layerDir, relPath string
		switch layerDesc.MediaType {
		case constants.ModelLayerMediaType:
			if !options.unpackConf.unpackModels {
				continue
			}
			layerDir, relPath, err = filesystem.VerifySubpath(options.unpackDir, config.Model.Path)
			if err != nil {
				return fmt.Errorf("Error resolving model path: %w", err)
			}
			output.Infof("Unpacking model to %s", relPath)

		case constants.CodeLayerMediaType:
			if !options.unpackConf.unpackCode {
				continue
			}
			codeEntry := config.Code[codeIdx]
			layerDir, relPath, err = filesystem.VerifySubpath(options.unpackDir, codeEntry.Path)
			if err != nil {
				return fmt.Errorf("Error resolving code path: %w", err)
			}
			output.Infof("Unpacking code to %s", relPath)
			codeIdx += 1

		case constants.DataSetLayerMediaType:
			if !options.unpackConf.unpackDatasets {
				continue
			}
			datasetEntry := config.DataSets[datasetIdx]
			layerDir, relPath, err = filesystem.VerifySubpath(options.unpackDir, datasetEntry.Path)
			if err != nil {
				return fmt.Errorf("Error resolving dataset path for dataset %s: %w", datasetEntry.Name, err)
			}
			output.Infof("Unpacking dataset %s to %s", datasetEntry.Name, relPath)
			datasetIdx += 1
		}
		if err := unpackLayer(ctx, store, layerDesc, layerDir, options.overwrite); err != nil {
			return err
		}
	}
	output.Debugf("Unpacked %d code layers", codeIdx)
	output.Debugf("Unpacked %d dataset layers", datasetIdx)

	return nil
}

func unpackConfig(config *artifact.KitFile, unpackDir string, overwrite bool) error {
	configPath := filepath.Join(unpackDir, constants.DefaultKitFileName)
	if fi, exists := filesystem.PathExists(configPath); exists {
		if !overwrite {
			return fmt.Errorf("failed to unpack config: path %s already exists", configPath)
		} else if !fi.Mode().IsRegular() {
			return fmt.Errorf("failed to unpack config: path %s exists and is not a regular file", configPath)
		}
	}

	configBytes, err := config.MarshalToYAML()
	if err != nil {
		return fmt.Errorf("failed to unpack config: %w", err)
	}

	output.Infof("Unpacking config to %s", configPath)
	if err := os.WriteFile(configPath, configBytes, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	return nil
}

func unpackLayer(ctx context.Context, store content.Storage, desc ocispec.Descriptor, unpackPath string, overwrite bool) error {
	rc, err := store.Fetch(ctx, desc)
	if err != nil {
		return fmt.Errorf("failed get layer %s: %w", desc.Digest, err)
	}
	defer rc.Close()

	gzr, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("error extracting gzipped file: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)

	if _, exists := filesystem.PathExists(unpackPath); exists {
		if !overwrite {
			return fmt.Errorf("failed to unpack: path %s already exists", unpackPath)
		}
		output.Debugf("Directory %s already exists", unpackPath)
	}
	unpackDir := filepath.Dir(unpackPath)
	if err := os.MkdirAll(unpackDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", unpackDir, err)
	}

	return extractTar(tr, unpackDir, overwrite)
}

func extractTar(tr *tar.Reader, dir string, overwrite bool) error {
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		outPath := filepath.Join(dir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if fi, exists := filesystem.PathExists(outPath); exists {
				if !overwrite {
					return fmt.Errorf("path '%s' already exists", outPath)
				}
				if !fi.IsDir() {
					return fmt.Errorf("path '%s' already exists and is not a directory", outPath)
				}
				output.Debugf("Path %s already exists", outPath)
			}
			output.Debugf("Creating directory %s", outPath)
			if err := os.MkdirAll(outPath, header.FileInfo().Mode()); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", outPath, err)
			}

		case tar.TypeReg:
			if fi, exists := filesystem.PathExists(outPath); exists {
				if !overwrite {
					return fmt.Errorf("path '%s' already exists", outPath)
				}
				if !fi.Mode().IsRegular() {
					return fmt.Errorf("path '%s' already exists and is not a regular file", outPath)
				}
			}
			output.Debugf("Extracting file %s", outPath)
			file, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, header.FileInfo().Mode())
			if err != nil {
				return fmt.Errorf("failed to create file %s: %w", outPath, err)
			}
			defer file.Close()

			written, err := io.Copy(file, tr)
			if err != nil {
				return fmt.Errorf("failed to write file %s: %w", outPath, err)
			}
			if written != header.Size {
				return fmt.Errorf("could not extract file %s", outPath)
			}

		default:
			return fmt.Errorf("Unrecognized type in archive: %s", header.Name)
		}
	}
	return nil
}
