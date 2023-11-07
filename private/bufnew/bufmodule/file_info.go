package bufmodule

import "github.com/bufbuild/buf/private/pkg/storage"

// FileInfo is the file info for a Module file.
//
// It comprises the typical storage.ObjectInfo, along with a pointer back to the Module.
// This allows callers to figure out i.e. the ModuleSetID, ModuleFullName, Commit, as well as any other
// data it may need.
type FileInfo interface {
	storage.ObjectInfo

	// Module returns the Module that contains this file.
	Module() Module
	// FileType returns the FileType of the file.
	//
	// This denotes if the File is a .proto file, documentation file, or license file.
	FileType() FileType

	isFileInfo()
}

// *** PRIVATE ***

type fileInfo struct {
	storage.ObjectInfo

	module   Module
	fileType FileType
}

func newFileInfo(
	objectInfo storage.ObjectInfo,
	module Module,
	fileType FileType,
) *fileInfo {
	return &fileInfo{
		ObjectInfo: objectInfo,
		module:     module,
		fileType:   fileType,
	}
}

func (f *fileInfo) Module() Module {
	return f.module
}

func (f *fileInfo) FileType() FileType {
	return f.fileType
}

func (*fileInfo) isFileInfo() {}