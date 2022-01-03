package postgres

import (
	"archive/tar"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/viper"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
	"github.com/wal-g/wal-g/utility"
)

// FileTarInterpreter extracts input to disk.
type FileTarInterpreter struct {
	DBDataDirectory string
	Sentinel        BackupSentinelDto
	FilesMetadata   FilesMetadataDto
	FilesToUnwrap   map[string]bool
	UnwrapResult    *UnwrapResult

	createNewIncrementalFiles bool
}

func NewFileTarInterpreter(
	dbDataDirectory string, sentinel BackupSentinelDto, filesMetadata FilesMetadataDto,
	filesToUnwrap map[string]bool, createNewIncrementalFiles bool,
) *FileTarInterpreter {
	return &FileTarInterpreter{dbDataDirectory, sentinel, filesMetadata,
		filesToUnwrap, NewUnwrapResult(), createNewIncrementalFiles}
}

// Interpret extracts a tar file to disk and creates needed directories.
// Returns the first error encountered. Calls fsync after each file
// is written successfully.
func (tarInterpreter *FileTarInterpreter) Interpret(fileReader io.Reader, fileInfo *tar.Header) error {
	tracelog.DebugLogger.Println("Interpreting: ", fileInfo.Name)
	targetPath := path.Join(tarInterpreter.DBDataDirectory, fileInfo.Name)
	fsync := !viper.GetBool(internal.TarDisableFsyncSetting)
	switch fileInfo.Typeflag {
	case tar.TypeReg, tar.TypeRegA:
		return tarInterpreter.unwrapRegularFile(fileReader, fileInfo, targetPath, fsync)
	case tar.TypeDir:
		err := os.MkdirAll(targetPath, 0755)
		if err != nil {
			return errors.Wrapf(err, "Interpret: failed to create all directories in %s", targetPath)
		}
		if err = os.Chmod(targetPath, os.FileMode(fileInfo.Mode)); err != nil {
			return errors.Wrap(err, "Interpret: chmod failed")
		}
	case tar.TypeLink:
		if err := os.Link(fileInfo.Name, targetPath); err != nil {
			return errors.Wrapf(err, "Interpret: failed to create hardlink %s", targetPath)
		}
	case tar.TypeSymlink:
		if err := os.Symlink(fileInfo.Name, targetPath); err != nil {
			return errors.Wrapf(err, "Interpret: failed to create symlink %s", targetPath)
		}
	}
	return nil
}

// PrepareDirs makes sure all dirs exist
func PrepareDirs(fileName string, targetPath string) error {
	if fileName == targetPath {
		return nil // because it runs in the local directory
	}
	base := filepath.Base(fileName)
	dir := strings.TrimSuffix(targetPath, base)
	err := os.MkdirAll(dir, 0755)
	return err
}

// TODO : unit tests
func (tarInterpreter *FileTarInterpreter) unwrapRegularFile(fileReader io.Reader,
	header *tar.Header,
	targetPath string,
	fsync bool) error {
	if tarInterpreter.FilesToUnwrap != nil {
		if _, ok := tarInterpreter.FilesToUnwrap[header.Name]; !ok {
			// don't have to unwrap it this time
			tracelog.DebugLogger.Printf("Don't have to unwrap '%s' this time\n", header.Name)
			return nil
		}
	}
	fileUnwrapper := getFileUnwrapper(tarInterpreter, header, targetPath)
	localFile, isNewFile, err := getLocalFile(targetPath, header)
	if err != nil {
		return err
	}
	defer utility.LoggedClose(localFile, "")
	defer utility.LoggedSync(localFile, "", fsync)
	var unwrapResult *FileUnwrapResult
	var unwrapError error
	if isNewFile {
		unwrapResult, unwrapError = fileUnwrapper.UnwrapNewFile(fileReader, header, localFile, fsync)
	} else {
		unwrapResult, unwrapError = fileUnwrapper.UnwrapExistingFile(fileReader, header, localFile, fsync)
	}
	if unwrapError != nil {
		return unwrapError
	}
	tarInterpreter.AddFileUnwrapResult(unwrapResult, header.Name)
	return nil
}

// get local file, create new if not existed
func getLocalFile(targetPath string, header *tar.Header) (localFile *os.File, isNewFile bool, err error) {
	if localFileInfo, _ := getLocalFileInfo(targetPath); localFileInfo != nil {
		localFile, err = os.OpenFile(targetPath, os.O_RDWR, 0666)
	} else {
		localFile, err = createLocalFile(targetPath, header.Name)
		isNewFile = true
	}
	return localFile, isNewFile, err
}

// get file unwrapper for file depending on backup type
func getFileUnwrapper(tarInterpreter *FileTarInterpreter, header *tar.Header, targetPath string) IBackupFileUnwrapper {
	fileDescription, haveFileDescription := tarInterpreter.FilesMetadata.Files[header.Name]
	isIncremented := haveFileDescription && fileDescription.IsIncremented
	var isPageFile bool
	if localFileInfo, _ := getLocalFileInfo(targetPath); localFileInfo != nil {
		isPageFile = isPagedFile(localFileInfo, targetPath)
	}
	options := &BackupFileOptions{isIncremented: isIncremented, isPageFile: isPageFile}

	// todo: clearer catchup backup detection logic
	isCatchup := tarInterpreter.createNewIncrementalFiles
	if isCatchup {
		return NewFileUnwrapper(CatchupBackupFileUnwrapper, options)
	}
	return NewFileUnwrapper(DefaultBackupFileUnwrapper, options)
}

// get file info by file path
func getLocalFileInfo(targetPath string) (fileInfo os.FileInfo, err error) {
	info, err := os.Stat(targetPath)
	if os.IsNotExist(err) {
		return nil, err
	}
	if info.IsDir() {
		return nil, errors.New("requested file is directory. Aborting")
	}
	return info, nil
}

// create new local file on disk
func createLocalFile(targetPath, name string) (*os.File, error) {
	err := PrepareDirs(name, targetPath)
	if err != nil {
		return nil, errors.Wrap(err, "Interpret: failed to create all directories")
	}
	file, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create new file: '%s'", targetPath)
	}
	return file, nil
}

func (tarInterpreter *FileTarInterpreter) AddFileUnwrapResult(result *FileUnwrapResult, fileName string) {
	switch result.FileUnwrapResultType {
	case Skipped:
		return
	case Completed:
		tarInterpreter.addToCompletedFiles(fileName)
	case CreatedFromIncrement:
		tarInterpreter.addToCreatedPageFiles(fileName, result.blockCount)
	case WroteIncrementBlocks:
		tarInterpreter.addToWrittenIncrementFiles(fileName, result.blockCount)
	}
}

func (tarInterpreter *FileTarInterpreter) addToCompletedFiles(fileName string) {
	tarInterpreter.UnwrapResult.completedFilesMutex.Lock()
	tarInterpreter.UnwrapResult.completedFiles = append(tarInterpreter.UnwrapResult.completedFiles, fileName)
	tarInterpreter.UnwrapResult.completedFilesMutex.Unlock()
}

func (tarInterpreter *FileTarInterpreter) addToCreatedPageFiles(fileName string, blocksToRestoreCount int64) {
	tarInterpreter.UnwrapResult.createdPageFilesMutex.Lock()
	tarInterpreter.UnwrapResult.createdPageFiles[fileName] = blocksToRestoreCount
	tarInterpreter.UnwrapResult.createdPageFilesMutex.Unlock()
}

func (tarInterpreter *FileTarInterpreter) addToWrittenIncrementFiles(fileName string, writtenBlocksCount int64) {
	tarInterpreter.UnwrapResult.writtenIncrementFilesMutex.Lock()
	tarInterpreter.UnwrapResult.writtenIncrementFiles[fileName] = writtenBlocksCount
	tarInterpreter.UnwrapResult.writtenIncrementFilesMutex.Unlock()
}
