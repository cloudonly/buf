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
	"fmt"
	"io/fs"

	modulev1beta1 "buf.build/gen/go/bufbuild/registry/protocolbuffers/go/buf/registry/module/v1beta1"
	"connectrpc.com/connect"
	"github.com/bufbuild/buf/private/bufpkg/bufapi"
	"github.com/bufbuild/buf/private/bufpkg/bufmodule"
	"github.com/bufbuild/buf/private/pkg/slicesext"
	"github.com/bufbuild/buf/private/pkg/storage"
	"github.com/bufbuild/buf/private/pkg/syserror"
	"go.uber.org/zap"
)

// NewModuleDataProvider returns a new ModuleDataProvider for the given API client.
//
// A warning is printed to the logger if a given Module is deprecated.
func NewModuleDataProvider(
	logger *zap.Logger,
	clientProvider interface {
		bufapi.DownloadServiceClientProvider
		bufapi.GraphServiceClientProvider
		bufapi.ModuleServiceClientProvider
		bufapi.OwnerServiceClientProvider
	},
) bufmodule.ModuleDataProvider {
	return newModuleDataProvider(logger, clientProvider)
}

// *** PRIVATE ***

type moduleDataProvider struct {
	logger         *zap.Logger
	clientProvider interface {
		bufapi.DownloadServiceClientProvider
		bufapi.ModuleServiceClientProvider
	}
	graphProvider bufmodule.GraphProvider
}

func newModuleDataProvider(
	logger *zap.Logger,
	clientProvider interface {
		bufapi.DownloadServiceClientProvider
		bufapi.GraphServiceClientProvider
		bufapi.ModuleServiceClientProvider
		bufapi.OwnerServiceClientProvider
	},
) *moduleDataProvider {
	return &moduleDataProvider{
		logger:         logger,
		clientProvider: clientProvider,
		graphProvider: NewGraphProvider(
			logger,
			clientProvider,
		),
	}
}

func (a *moduleDataProvider) GetModuleDatasForModuleKeys(
	ctx context.Context,
	moduleKeys []bufmodule.ModuleKey,
) ([]bufmodule.ModuleData, error) {
	if _, err := bufmodule.ModuleFullNameStringToUniqueValue(moduleKeys); err != nil {
		return nil, err
	}

	// We don't want to persist this across calls - this could grow over time and this cache
	// isn't an LRU cache, and the information also may change over time.
	protoModuleProvider := newProtoModuleProvider(a.logger, a.clientProvider)

	registryToIndexedModuleKeys := getKeyToIndexedValues(
		moduleKeys,
		func(moduleKey bufmodule.ModuleKey) string {
			return moduleKey.ModuleFullName().Registry()
		},
	)
	moduleDatas := make([]bufmodule.ModuleData, 0, len(moduleKeys))
	for registry, indexedModuleKeys := range registryToIndexedModuleKeys {
		// registryModuleDatas are in the same order as indexedModuleKeys.
		registryModuleDatas, err := a.getModuleDatasForRegistryAndModuleKeys(
			ctx,
			protoModuleProvider,
			registry,
			getValuesForIndexedValues(indexedModuleKeys),
		)
		if err != nil {
			return nil, err
		}
		for i, registryModuleData := range registryModuleDatas {
			moduleDatas[indexedModuleKeys[i].Index] = registryModuleData
		}
	}
	return moduleDatas, nil
}

// Returns ModuleDatas in the same order as the input ModuleKeys
func (a *moduleDataProvider) getModuleDatasForRegistryAndModuleKeys(
	ctx context.Context,
	protoModuleProvider *protoModuleProvider,
	registry string,
	moduleKeys []bufmodule.ModuleKey,
) ([]bufmodule.ModuleData, error) {
	graph, err := a.graphProvider.GetGraphForModuleKeys(ctx, moduleKeys)
	if err != nil {
		return nil, err
	}
	commitIDToIndexedModuleKey, err := getKeyToUniqueIndexedValue(
		moduleKeys,
		func(moduleKey bufmodule.ModuleKey) string {
			return moduleKey.CommitID()
		},
	)
	if err != nil {
		return nil, err
	}
	commitIDToProtoContent, err := a.getCommitIDToProtoContentForRegistryAndModuleKeys(
		ctx,
		protoModuleProvider,
		registry,
		commitIDToIndexedModuleKey,
	)
	if err != nil {
		return nil, err
	}
	moduleDatas := make([]bufmodule.ModuleData, 0, len(moduleKeys))
	if err := graph.WalkNodes(
		func(
			moduleKey bufmodule.ModuleKey,
			_ []bufmodule.ModuleKey,
			depModuleKeys []bufmodule.ModuleKey,
		) error {
			protoContent, ok := commitIDToProtoContent[moduleKey.CommitID()]
			if !ok {
				// We only care to get content for a subset of the graph. If we have something
				// in the graph without content, we just skip it.
				return nil
			}
			indexedModuleKey, ok := commitIDToIndexedModuleKey[moduleKey.CommitID()]
			if !ok {
				return syserror.Newf("could not find indexed ModuleKey for commit ID %q", moduleKey.CommitID())
			}
			moduleDatas[indexedModuleKey.Index] = bufmodule.NewModuleData(
				ctx,
				moduleKey,
				func() (storage.ReadBucket, error) {
					return protoFilesToBucket(protoContent.Files)
				},
				func() ([]bufmodule.ModuleKey, error) { return depModuleKeys, nil },
				func() (bufmodule.ObjectData, error) {
					return protoFileToObjectData(protoContent.BufYamlFile)
				},
				func() (bufmodule.ObjectData, error) {
					return protoFileToObjectData(protoContent.BufLockFile)
				},
			)
			return nil
		},
	); err != nil {
		return nil, err
	}
	return moduleDatas, nil
}

func (a *moduleDataProvider) getCommitIDToProtoContentForRegistryAndModuleKeys(
	ctx context.Context,
	protoModuleProvider *protoModuleProvider,
	registry string,
	commitIDToIndexedModuleKey map[string]*indexedValue[bufmodule.ModuleKey],
) (map[string]*modulev1beta1.DownloadResponse_Content, error) {
	protoCommitIDs, err := slicesext.MapError(
		slicesext.MapKeysToSortedSlice(commitIDToIndexedModuleKey),
		func(commitID string) (string, error) {
			return CommitIDToProto(commitID)
		},
	)
	if err != nil {
		return nil, err
	}

	response, err := a.clientProvider.DownloadServiceClient(registry).Download(
		ctx,
		connect.NewRequest(
			&modulev1beta1.DownloadRequest{
				// TODO: chunking
				Values: slicesext.Map(
					protoCommitIDs,
					func(protoCommitID string) *modulev1beta1.DownloadRequest_Value {
						return &modulev1beta1.DownloadRequest_Value{
							ResourceRef: &modulev1beta1.ResourceRef{
								Value: &modulev1beta1.ResourceRef_Id{
									Id: protoCommitID,
								},
							},
							DigestType: modulev1beta1.DigestType_DIGEST_TYPE_B5,
						}
					},
				),
			},
		),
	)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			// Kind of an abuse of fs.PathError. Is there a way to get a specific ModuleKey out of this?
			return nil, &fs.PathError{Op: "read", Path: err.Error(), Err: fs.ErrNotExist}
		}
		return nil, err
	}
	if len(response.Msg.Contents) != len(commitIDToIndexedModuleKey) {
		return nil, fmt.Errorf("expected %d Contents, got %d", len(commitIDToIndexedModuleKey), len(response.Msg.Contents))
	}
	commitIDToProtoContent, err := slicesext.ToUniqueValuesMapError(
		response.Msg.Contents,
		func(protoContent *modulev1beta1.DownloadResponse_Content) (string, error) {
			return CommitIDToProto(protoContent.Commit.Id)
		},
	)
	if err != nil {
		return nil, err
	}
	for commitID, indexedModuleKey := range commitIDToIndexedModuleKey {
		protoContent, ok := commitIDToProtoContent[commitID]
		if !ok {
			return nil, fmt.Errorf("no content returned for commit ID %s", commitID)
		}
		if err := a.warnIfDeprecated(
			ctx,
			protoModuleProvider,
			registry,
			protoContent.Commit,
			indexedModuleKey.Value,
		); err != nil {
			return nil, err
		}
	}
	return commitIDToProtoContent, nil
}

// In the future, we might want to add State, Visibility, etc as parameters to bufmodule.Module, to
// match what we are doing with Commit and Graph to some degree, and then bring this warning
// out of the ModuleDataProvider. However, if we did this, this has unintended consequences - right now,
// by this being here, we only warn when we don't have the module in the cache, which we sort of want?
// State is a property only on the BSR, it's not a property on a per-commit basis, so this gets into
// weird territory.
func (a *moduleDataProvider) warnIfDeprecated(
	ctx context.Context,
	protoModuleProvider *protoModuleProvider,
	registry string,
	protoCommit *modulev1beta1.Commit,
	moduleKey bufmodule.ModuleKey,
) error {
	protoModule, err := protoModuleProvider.getProtoModuleForModuleID(
		ctx,
		registry,
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

func getCommitIDToBucketForProtoContents(
	protoContents []*modulev1beta1.DownloadResponse_Content,
) (map[string]storage.ReadBucket, error) {
	commitIDToBucket := make(map[string]storage.ReadBucket, len(protoContents))
	for _, protoContent := range protoContents {
		commitID, err := ProtoToCommitID(protoContent.Commit.Id)
		if err != nil {
			return nil, err
		}
		bucket, err := protoFilesToBucket(protoContent.Files)
		if err != nil {
			return nil, err
		}
		commitIDToBucket[commitID] = bucket
	}
	return commitIDToBucket, nil
}
