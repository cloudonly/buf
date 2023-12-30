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

package bufmoduletesting

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/bufbuild/buf/private/bufpkg/bufmodule"
	"github.com/bufbuild/buf/private/pkg/slicesext"
	"github.com/bufbuild/buf/private/pkg/storage"
	"github.com/bufbuild/buf/private/pkg/storage/storagemem"
	"github.com/bufbuild/buf/private/pkg/storage/storageos"
	"github.com/bufbuild/buf/private/pkg/uuidutil"
	"go.uber.org/zap"
)

// 2023-01-01 at 12:00 UTC
var mockTime = time.Unix(1672574400, 0)

// ModuleData is the data needed to construct a Module in test.
//
// Exactly one of PathToData, Bucket, DirPath must be set.
//
// Name is the ModuleFullName string. When creating an OmniProvider, Name is required.
//
// CommitID is optional, but it must be unique across all ModuleDatas. If CommitID is not set,
// a mock commitID is created if Name is set.
//
// CreateTime is optional. If CreateTime is not set, a mock create Time is created. This create
// time is the same for all data without a Time.
type ModuleData struct {
	Name        string
	CommitID    string
	CreateTime  time.Time
	DirPath     string
	PathToData  map[string][]byte
	Bucket      storage.ReadBucket
	NotTargeted bool
}

// OmniProvider is a ModuleKeyProvider, ModuleDataProvider, and ModuleSet for testing.
type OmniProvider interface {
	bufmodule.ModuleKeyProvider
	bufmodule.ModuleDataProvider
	bufmodule.CommitProvider
	bufmodule.ModuleSet
}

// NewOmniProvider returns a new OmniProvider.
//
// Note the ModuleDatas must be self-contained, that is they only import from each other.
func NewOmniProvider(
	moduleDatas ...ModuleData,
) (OmniProvider, error) {
	return newOmniProvider(moduleDatas)
}

// NewModuleSet returns a new ModuleSet.
//
// This can be used in cases where ModuleKeyProviders and ModuleDataProviders are not needed,
// and when ModuleFullNames do not matter.
//
// Note the ModuleDatas must be self-contained, that is they only import from each other.
func NewModuleSet(
	moduleDatas ...ModuleData,
) (bufmodule.ModuleSet, error) {
	return newModuleSet(moduleDatas, false, nil)
}

// NewModuleSetForDirPath returns a new ModuleSet for the directory path.
//
// This can be used in cases where ModuleKeyProviders and ModuleDataProviders are not needed,
// and when ModuleFullNames do not matter.
//
// Note that this Module cannot have any dependencies.
func NewModuleSetForDirPath(
	dirPath string,
) (bufmodule.ModuleSet, error) {
	return NewModuleSet(
		ModuleData{
			DirPath: dirPath,
		},
	)
}

// NewModuleSetForPathToData returns a new ModuleSet for the path to data map.
//
// This can be used in cases where ModuleKeyProviders and ModuleDataProviders are not needed,
// and when ModuleFullNames do not matter.
//
// Note that this Module cannot have any dependencies.
func NewModuleSetForPathToData(
	pathToData map[string][]byte,
) (bufmodule.ModuleSet, error) {
	return NewModuleSet(
		ModuleData{
			PathToData: pathToData,
		},
	)
}

// NewModuleSetForBucket returns a new ModuleSet for the Bucket.
//
// This can be used in cases where ModuleKeyProviders and ModuleDataProviders are not needed,
// and when ModuleFullNames do not matter.
//
// Note that this Module cannot have any dependencies.
func NewModuleSetForBucket(
	bucket storage.ReadBucket,
) (bufmodule.ModuleSet, error) {
	return NewModuleSet(
		ModuleData{
			Bucket: bucket,
		},
	)
}

// NewCommitID returns a new CommitID.
//
// This is a dashless UUID.
func NewCommitID() (string, error) {
	id, err := uuidutil.New()
	if err != nil {
		return "", err
	}
	return uuidutil.ToDashless(id)
}

// *** PRIVATE ***

type omniProvider struct {
	bufmodule.ModuleSet
	commitIDToCreateTime map[string]time.Time
}

func newOmniProvider(
	moduleDatas []ModuleData,
) (*omniProvider, error) {
	commitIDToCreateTime := make(map[string]time.Time)
	moduleSet, err := newModuleSet(moduleDatas, true, commitIDToCreateTime)
	if err != nil {
		return nil, err
	}
	return &omniProvider{
		ModuleSet:            moduleSet,
		commitIDToCreateTime: commitIDToCreateTime,
	}, nil
}

func (o *omniProvider) GetModuleKeysForModuleRefs(
	ctx context.Context,
	moduleRefs []bufmodule.ModuleRef,
) ([]bufmodule.ModuleKey, error) {
	moduleKeys := make([]bufmodule.ModuleKey, len(moduleRefs))
	for i, moduleRef := range moduleRefs {
		module := o.GetModuleForModuleFullName(moduleRef.ModuleFullName())
		if module == nil {
			return nil, &fs.PathError{Op: "read", Path: moduleRef.String(), Err: fs.ErrNotExist}
		}
		moduleKey, err := bufmodule.ModuleToModuleKey(module)
		if err != nil {
			return nil, err
		}
		moduleKeys[i] = moduleKey
	}
	return moduleKeys, nil
}

func (o *omniProvider) GetModuleDatasForModuleKeys(
	ctx context.Context,
	moduleKeys []bufmodule.ModuleKey,
	options ...bufmodule.GetModuleDatasForModuleKeysOption,
) ([]bufmodule.ModuleData, error) {
	duplicateModuleConfigFullNameStrings := slicesext.Duplicates(
		slicesext.Map(
			moduleKeys,
			func(moduleKey bufmodule.ModuleKey) string {
				return moduleKey.ModuleFullName().String()
			},
		),
	)
	if len(duplicateModuleConfigFullNameStrings) > 0 {
		return nil, fmt.Errorf("module names %q seen more than once", strings.Join(duplicateModuleConfigFullNameStrings, ", "))
	}

	getModuleDatasForModuleKeysOptions := bufmodule.NewGetModuleDatasForModuleKeysOptions(options)
	moduleFullNameStringToModuleData := make(map[string]bufmodule.ModuleData, len(moduleKeys))
	for _, moduleKey := range moduleKeys {
		if err := o.populateModuleDatasForModuleKeyRec(
			ctx,
			moduleFullNameStringToModuleData,
			moduleKey,
			getModuleDatasForModuleKeysOptions.IncludeDepModuleDatas(),
		); err != nil {
			return nil, err
		}
	}
	moduleDatas := slicesext.MapValuesToSlice(moduleFullNameStringToModuleData)
	sort.Slice(
		moduleDatas,
		func(i int, j int) bool {
			return moduleDatas[i].ModuleKey().ModuleFullName().String() < moduleDatas[j].ModuleKey().ModuleFullName().String()
		},
	)
	return moduleDatas, nil
}

func (o *omniProvider) GetCommitsForModuleKeys(
	ctx context.Context,
	moduleKeys []bufmodule.ModuleKey,
) ([]bufmodule.Commit, error) {
	commits := make([]bufmodule.Commit, len(moduleKeys))
	for i, moduleKey := range moduleKeys {
		createTime, ok := o.commitIDToCreateTime[moduleKey.CommitID()]
		if !ok {
			return nil, &fs.PathError{Op: "read", Path: moduleKey.String(), Err: fs.ErrNotExist}
		}
		commits[i] = bufmodule.NewCommit(
			moduleKey,
			func() (time.Time, error) {
				return createTime, nil
			},
		)
	}
	return commits, nil
}

func (o *omniProvider) populateModuleDatasForModuleKeyRec(
	ctx context.Context,
	moduleFullNameStringToModuleData map[string]bufmodule.ModuleData,
	moduleKey bufmodule.ModuleKey,
	includeDepModuleDatas bool,
) error {
	if _, ok := moduleFullNameStringToModuleData[moduleKey.ModuleFullName().String()]; ok {
		return nil
	}

	module := o.GetModuleForModuleFullName(moduleKey.ModuleFullName())
	if module == nil {
		return &fs.PathError{Op: "read", Path: moduleKey.String(), Err: fs.ErrNotExist}
	}
	moduleDeps, err := module.ModuleDeps()
	if err != nil {
		return err
	}
	declaredDepModuleKeys, err := slicesext.MapError(

		moduleDeps,
		func(moduleDep bufmodule.ModuleDep) (bufmodule.ModuleKey, error) {
			return bufmodule.ModuleToModuleKey(moduleDep)
		},
	)
	if err != nil {
		return err
	}
	moduleData := bufmodule.NewModuleData(
		ctx,
		moduleKey,
		func() (storage.ReadBucket, error) {
			return bufmodule.ModuleReadBucketToStorageReadBucket(module), nil
		},
		func() ([]bufmodule.ModuleKey, error) {
			return declaredDepModuleKeys, nil
		},
	)
	moduleFullNameStringToModuleData[moduleKey.ModuleFullName().String()] = moduleData

	if includeDepModuleDatas {
		for _, declaredDepModuleKey := range declaredDepModuleKeys {
			if err := o.populateModuleDatasForModuleKeyRec(
				ctx,
				moduleFullNameStringToModuleData,
				declaredDepModuleKey,
				includeDepModuleDatas,
			); err != nil {
				return err
			}
		}
	}

	return nil
}

func newModuleSet(
	moduleDatas []ModuleData,
	requireName bool,
	// may be nil
	commitIDToCreateTime map[string]time.Time,
) (bufmodule.ModuleSet, error) {
	moduleSetBuilder := bufmodule.NewModuleSetBuilder(context.Background(), zap.NewNop(), bufmodule.NopModuleDataProvider)
	for i, moduleData := range moduleDatas {
		if err := addModuleDataToModuleSetBuilder(
			moduleSetBuilder,
			moduleData,
			requireName,
			commitIDToCreateTime,
			i,
		); err != nil {
			return nil, err
		}
	}
	return moduleSetBuilder.Build()
}

func addModuleDataToModuleSetBuilder(
	moduleSetBuilder bufmodule.ModuleSetBuilder,
	moduleData ModuleData,
	requireName bool,
	// may be nil
	commitIDToCreateTime map[string]time.Time,
	index int,
) error {
	if boolCount(
		moduleData.DirPath != "",
		moduleData.PathToData != nil,
		moduleData.Bucket != nil,
	) != 1 {
		return errors.New("exactly one of Bucket, PathToData, DirPath must be set on ModuleData")
	}
	var bucket storage.ReadBucket
	var bucketID string
	var err error
	switch {
	case moduleData.DirPath != "":
		storageosProvider := storageos.NewProvider(storageos.ProviderWithSymlinks())
		bucket, err = storageosProvider.NewReadWriteBucket(
			moduleData.DirPath,
			storageos.ReadWriteBucketWithSymlinksIfSupported(),
		)
		if err != nil {
			return err
		}
		bucketID = moduleData.DirPath
	case moduleData.PathToData != nil:
		bucket, err = storagemem.NewReadBucket(moduleData.PathToData)
		if err != nil {
			return err
		}
		bucketID = fmt.Sprintf("omniProviderBucket-%d", index)
	case moduleData.Bucket != nil:
		bucket = moduleData.Bucket
		bucketID = fmt.Sprintf("omniProviderBucket-%d", index)
	default:
		// Should never get here.
		return errors.New("boolCount returned 1 but all ModuleData fields were nil")
	}
	var localModuleOptions []bufmodule.LocalModuleOption
	if moduleData.Name != "" {
		moduleFullName, err := bufmodule.ParseModuleFullName(moduleData.Name)
		if err != nil {
			return err
		}
		commitID := moduleData.CommitID
		if commitID == "" {
			commitID, err = NewCommitID()
			if err != nil {
				return err
			}
		}
		if commitIDToCreateTime != nil {
			createTime := moduleData.CreateTime
			if createTime.IsZero() {
				createTime = mockTime
			}
			commitIDToCreateTime[commitID] = createTime
		}
		localModuleOptions = []bufmodule.LocalModuleOption{
			bufmodule.LocalModuleWithModuleFullNameAndCommitID(moduleFullName, commitID),
		}
	} else if requireName {
		return errors.New("ModuleData.Name was required in this context")
	}
	moduleSetBuilder.AddLocalModule(
		bucket,
		bucketID,
		!moduleData.NotTargeted,
		localModuleOptions...,
	)
	return nil
}

func boolCount(bools ...bool) int {
	return slicesext.Count(bools, func(value bool) bool { return value })
}
