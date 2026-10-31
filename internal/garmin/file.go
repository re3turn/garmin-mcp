package garmin

import (
	"os"
	"path/filepath"
)

// replaceFile writes through a temp file (mode 0600) and a rename, so a failed
// write never leaves a truncated file. On Windows the rename fails while
// another process has the file open; retrying works. A symlink at path is
// replaced, not followed, so a download can't escape its folder through one.
func replaceFile(path string, content []byte, dirPerm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return err
	}
	// Without Sync, a crash right after the rename can leave an empty file
	// under the final name on filesystems that delay data writes.
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
