// Copyright 2020-2024 Buf Technologies, Inc.
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
)

func getProtoCommitForRegistryAndCommitID(
	ctx context.Context,
	clientProvider bufapi.CommitServiceClientProvider,
	registry string,
	commitID string,
	digestType bufmodule.DigestType,
) (*modulev1beta1.Commit, error) {
	protoCommits, err := getProtoCommitsForRegistryAndCommitIDs(ctx, clientProvider, registry, []string{commitID}, digestType)
	if err != nil {
		return nil, err
	}
	// We already do length checking in getProtoCommitsForRegistryAndCommitIDs.
	return protoCommits[0], nil
}

func getProtoCommitsForRegistryAndCommitIDs(
	ctx context.Context,
	clientProvider bufapi.CommitServiceClientProvider,
	registry string,
	commitIDs []string,
	digestType bufmodule.DigestType,
) ([]*modulev1beta1.Commit, error) {
	protoCommitIDs, err := slicesext.MapError(
		commitIDs,
		func(commitID string) (string, error) {
			return CommitIDToProto(commitID)
		},
	)
	if err != nil {
		return nil, err
	}
	protoDigestType, err := digestTypeToProto(digestType)
	if err != nil {
		return nil, err
	}
	response, err := clientProvider.CommitServiceClient(registry).GetCommits(
		ctx,
		connect.NewRequest(
			&modulev1beta1.GetCommitsRequest{
				// TODO: chunking
				ResourceRefs: slicesext.Map(
					protoCommitIDs,
					func(protoCommitID string) *modulev1beta1.ResourceRef {
						return &modulev1beta1.ResourceRef{
							Value: &modulev1beta1.ResourceRef_Id{
								Id: protoCommitID,
							},
						}
					},
				),
				DigestType: protoDigestType,
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
	if len(response.Msg.Commits) != len(commitIDs) {
		return nil, fmt.Errorf("expected %d Commits, got %d", len(commitIDs), len(response.Msg.Commits))
	}
	return response.Msg.Commits, nil
}