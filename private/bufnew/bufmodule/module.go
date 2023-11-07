package bufmodule

import (
	"context"
	"fmt"
	"sync"

	"github.com/bufbuild/buf/private/bufpkg/bufcas"
)

// Module presents a BSR module.
type Module interface {
	// ModuleInfo contains a Module's optional ModuleFullName, optional commit ID, and Digest.
	ModuleInfo

	// ModuleReadBucket allows for reading of a Module's files.
	//
	// A Module consists of .proto files, documentation file(s), and license file(s). All of these
	// are accessible via the functions on ModuleReadBucket. As such, the FileTypes() function will
	// return FileTypeProto, FileTypeDoc, FileTypeLicense.
	//
	// This bucket is not self-contained - it requires the files from dependencies to be so. As such,
	// IsProtoFilesSelfContained() returns false.
	//
	// This package currently exposes functionality to walk just the .proto files, and get the singular
	// documentation and license files, via WalkProtoFileInfos, GetDocFile, and GetLicenseFile.
	//
	// GetDocFile and GetLicenseFile may change in the future if other paths are accepted for
	// documentation or licenses, or if we allow multiple documentation or license files to
	// exist within a Module (currently, only one of each is allowed).
	ModuleReadBucket

	// DepModules returns the dependency list for this specific module.
	//
	// This list is pruned - only Modules that this Module actually depends on via import statements
	// within its .proto files will be returned.
	//
	// Dependencies with the same ModuleFullName will always have the same commits and digests.
	DepModules(ctx context.Context) ([]Module, error)

	isModule()
}

// ModuleDigestB5 computes a b5 Digest for the given Module.
//
// A Module Digest is a composite Digest of all Module Files, and all Module dependencies.
//
// All Files are added to a bufcas.Manifest, which is then turned into a bufcas.Blob.
// The Digest of the Blob, along with all Digests of the dependencies, are then sorted,
// and then digested themselves as content.
//
// Note that the name of the Module and any of its dependencies has no effect on the Digest.
func ModuleDigestB5(ctx context.Context, module Module) (bufcas.Digest, error) {
	fileDigest, err := moduleReadBucketDigestB5(ctx, module)
	if err != nil {
		return nil, err
	}
	depModules, err := module.DepModules(ctx)
	if err != nil {
		return nil, err
	}
	digests := []bufcas.Digest{fileDigest}
	for _, depModule := range depModules {
		digest, err := depModule.Digest(ctx)
		if err != nil {
			return nil, err
		}
		digests = append(digests, digest)
	}

	// NewDigestForDigests deals with sorting.
	return bufcas.NewDigestForDigests(digests)
}

// *** PRIVATE ***

// module

type module struct {
	ModuleInfo
	ModuleReadBucket

	depModules []Module
}

func newModule(
	moduleInfo ModuleInfo,
	moduleReadBucket ModuleReadBucket,
	depModules []Module,
) *module {
	return &module{
		ModuleInfo:       moduleInfo,
		ModuleReadBucket: moduleReadBucket,
		depModules:       depModules,
	}
}

func (m *module) DepModules(context.Context) ([]Module, error) {
	return m.depModules, nil
}

func (*module) isModule() {}

// lazyModule

type lazyModule struct {
	ModuleInfo

	getModuleFunc func() (Module, error)
}

func newLazyModule(
	moduleInfo ModuleInfo,
	getModuleFunc func() (Module, error),
) Module {
	return &lazyModule{
		ModuleInfo:    moduleInfo,
		getModuleFunc: sync.OnceValues(getModuleFunc),
	}
}

func (m *lazyModule) GetFile(ctx context.Context, path string) (File, error) {
	module, err := m.getModule(ctx)
	if err != nil {
		return nil, err
	}
	return module.GetFile(ctx, path)
}

func (m *lazyModule) StatFileInfo(ctx context.Context, path string) (FileInfo, error) {
	module, err := m.getModule(ctx)
	if err != nil {
		return nil, err
	}
	return module.StatFileInfo(ctx, path)
}

func (m *lazyModule) WalkFileInfos(ctx context.Context, f func(FileInfo) error) error {
	module, err := m.getModule(ctx)
	if err != nil {
		return err
	}
	return module.WalkFileInfos(ctx, f)
}

func (m *lazyModule) DepModules(ctx context.Context) ([]Module, error) {
	module, err := m.getModule(ctx)
	if err != nil {
		return nil, err
	}
	return module.DepModules(ctx)
}

func (m *lazyModule) getModule(ctx context.Context) (Module, error) {
	module, err := m.getModuleFunc()
	if err != nil {
		return nil, err
	}
	expectedDigest, err := m.ModuleInfo.Digest(ctx)
	if err != nil {
		return nil, err
	}
	actualDigest, err := module.Digest(ctx)
	if err != nil {
		return nil, err
	}
	if !bufcas.DigestEqual(expectedDigest, actualDigest) {
		return nil, fmt.Errorf("expected digest %v, got %v", expectedDigest, actualDigest)
	}
	return module, nil
}

func (*lazyModule) isModuleReadBucket() {}
func (*lazyModule) isModule()           {}
