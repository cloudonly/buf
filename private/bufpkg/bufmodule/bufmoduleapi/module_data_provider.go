// Copyright 2020-2023 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bufmoduleapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	modulev1beta1 "buf.build/gen/go/bufbuild/registry/protocolbuffers/go/buf/registry/module/v1beta1"
	"connectrpc.com/connect"
	"github.com/bufbuild/buf/private/bufpkg/bufapi"
	"github.com/bufbuild/buf/private/bufpkg/bufmodule"
	"github.com/bufbuild/buf/private/pkg/slicesext"
	"github.com/bufbuild/buf/private/pkg/storage"
	"go.uber.org/zap"
)

// NewModuleDataProvider returns a new ModuleDataProvider for the given API client.
//
// A warning is printed to the logger if a given Module is deprecated.
func NewModuleDataProvider(
	logger *zap.Logger,
	clientProvider bufapi.ClientProvider,
) bufmodule.ModuleDataProvider {
	return newModuleDataProvider(logger, clientProvider)
}

// *** PRIVATE ***

type moduleDataProvider struct {
	logger         *zap.Logger
	clientProvider bufapi.ClientProvider
}

func newModuleDataProvider(
	logger *zap.Logger,
	clientProvider bufapi.ClientProvider,
) *moduleDataProvider {
	return &moduleDataProvider{
		logger:         logger,
		clientProvider: clientProvider,
	}
}

func (a *moduleDataProvider) GetOptionalModuleDatasForModuleKeys(
	ctx context.Context,
	moduleKeys ...bufmodule.ModuleKey,
) ([]bufmodule.OptionalModuleData, error) {
	// We don't want to persist these across calls - this could grow over time and this cache
	// isn't an LRU cache, and the information also may change over time.
	protoModuleProvider := newProtoModuleProvider(a.logger, a.clientProvider)
	protoOwnerProvider := newProtoOwnerProvider(a.logger, a.clientProvider)
	// TODO: Do the work to coalesce ModuleKeys by registry hostname, make calls out to the CommitService
	// per registry, then get back the resulting data, and order it in the same order as the input ModuleKeys.
	// Make sure to respect 250 max.
	optionalModuleDatas := make([]bufmodule.OptionalModuleData, len(moduleKeys))
	for i, moduleKey := range moduleKeys {
		moduleData, err := a.getModuleDataForModuleKey(
			ctx,
			protoModuleProvider,
			protoOwnerProvider,
			moduleKey,
		)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
		optionalModuleDatas[i] = bufmodule.NewOptionalModuleData(moduleData)
	}
	return optionalModuleDatas, nil
}

func (a *moduleDataProvider) getModuleDataForModuleKey(
	ctx context.Context,
	protoModuleProvider *protoModuleProvider,
	protoOwnerProvider *protoOwnerProvider,
	moduleKey bufmodule.ModuleKey,
) (bufmodule.ModuleData, error) {
	registryHostname := moduleKey.ModuleFullName().Registry()

	protoCommitID, err := CommitIDToProto(moduleKey.CommitID())
	if err != nil {
		return nil, err
	}
	response, err := a.clientProvider.DownloadServiceClient(registryHostname).Download(
		ctx,
		connect.NewRequest(
			&modulev1beta1.DownloadRequest{
				Values: []*modulev1beta1.DownloadRequest_Value{
					{
						ResourceRef: &modulev1beta1.ResourceRef{
							Value: &modulev1beta1.ResourceRef_Id{
								Id: protoCommitID,
							},
						},
					},
				},
				DigestType: modulev1beta1.DigestType_DIGEST_TYPE_B5,
			},
		),
	)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, &fs.PathError{Op: "read", Path: moduleKey.ModuleFullName().String(), Err: fs.ErrNotExist}
		}
		return nil, err
	}
	if len(response.Msg.References) != 1 {
		return nil, fmt.Errorf("expected 1 Reference, got %d", len(response.Msg.References))
	}
	protoCommitIDToCommit, err := getProtoCommitIDToCommitForProtoDownloadResponse(response.Msg)
	if err != nil {
		return nil, err
	}
	protoCommitIDToBucket, err := getProtoCommitIDToBucketForProtoDownloadResponse(response.Msg)
	if err != nil {
		return nil, err
	}
	if err := a.warnIfDeprecated(
		ctx,
		protoModuleProvider,
		protoCommitIDToCommit,
		registryHostname,
		moduleKey,
		response.Msg.References[0],
	); err != nil {
		return nil, err
	}
	return getModuleDataForProtoDownloadResponseReference(
		ctx,
		protoModuleProvider,
		protoOwnerProvider,
		protoCommitIDToCommit,
		protoCommitIDToBucket,
		registryHostname,
		moduleKey,
		response.Msg.References[0],
	)
}

func (a *moduleDataProvider) warnIfDeprecated(
	ctx context.Context,
	protoModuleProvider *protoModuleProvider,
	protoCommitIDToCommit map[string]*modulev1beta1.Commit,
	registryHostname string,
	moduleKey bufmodule.ModuleKey,
	protoReference *modulev1beta1.DownloadResponse_Reference,
) error {
	protoCommit, ok := protoCommitIDToCommit[protoReference.CommitId]
	if !ok {
		return fmt.Errorf("commit_id %q was not present in Commits on DownloadModuleResponse", protoReference.CommitId)
	}
	protoModule, err := protoModuleProvider.getProtoModuleForModuleID(
		ctx,
		registryHostname,
		protoCommit.ModuleId,
	)
	if err != nil {
		return err
	}
	if protoModule.State == modulev1beta1.ModuleState_MODULE_STATE_DEPRECATED {
		a.logger.Warn(fmt.Sprintf("%s is deprecated", moduleKey.ModuleFullName().String()))
	}
	return nil
}

func getModuleDataForProtoDownloadResponseReference(
	ctx context.Context,
	protoModuleProvider *protoModuleProvider,
	protoOwnerProvider *protoOwnerProvider,
	protoCommitIDToCommit map[string]*modulev1beta1.Commit,
	protoCommitIDToBucket map[string]storage.ReadBucket,
	registryHostname string,
	moduleKey bufmodule.ModuleKey,
	protoReference *modulev1beta1.DownloadResponse_Reference,
) (bufmodule.ModuleData, error) {
	bucket, ok := protoCommitIDToBucket[protoReference.CommitId]
	if !ok {
		return nil, fmt.Errorf("commit_id %q was not present in Contents on DownloadModuleResponse", protoReference.CommitId)
	}
	depProtoCommits, err := slicesext.MapError(
		protoReference.DepCommitIds,
		func(protoCommitID string) (*modulev1beta1.Commit, error) {
			commit, ok := protoCommitIDToCommit[protoCommitID]
			if !ok {
				return nil, fmt.Errorf("dep_commit_id %q was not present in Commits on DownloadModuleResponse", protoCommitID)
			}
			return commit, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return bufmodule.NewModuleData(
		ctx,
		moduleKey,
		func() (storage.ReadBucket, error) {
			return bucket, nil
		},
		func() ([]bufmodule.ModuleKey, error) {
			return getModuleKeysForProtoCommits(
				ctx,
				protoModuleProvider,
				protoOwnerProvider,
				registryHostname,
				depProtoCommits,
			)
		},
	), nil
}

func getProtoCommitIDToCommitForProtoDownloadResponse(
	protoDownloadResponse *modulev1beta1.DownloadResponse,
) (map[string]*modulev1beta1.Commit, error) {
	return slicesext.ToUniqueValuesMapError(
		protoDownloadResponse.Commits,
		func(protoCommit *modulev1beta1.Commit) (string, error) {
			return protoCommit.Id, nil
		},
	)
}

func getProtoCommitIDToBucketForProtoDownloadResponse(
	protoDownloadResponse *modulev1beta1.DownloadResponse,
) (map[string]storage.ReadBucket, error) {
	protoCommitIDToBucket := make(map[string]storage.ReadBucket, len(protoDownloadResponse.Contents))
	for _, protoContent := range protoDownloadResponse.Contents {
		bucket, err := protoFilesToBucket(protoContent.Files)
		if err != nil {
			return nil, err
		}
		protoCommitIDToBucket[protoContent.CommitId] = bucket
	}
	return protoCommitIDToBucket, nil
}
