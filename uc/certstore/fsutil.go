package certstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// tmpSuffix marks a file that is not yet complete.
const tmpSuffix = ".tmp"

// writeFileSync writes data to path through path+".tmp": create, write,
// set mode, fsync, close, rename. A reader therefore sees the old file or
// the complete new file. The caller syncs the directory. afterTemp, when
// set, runs after the temporary file is synced and before the rename.
func writeFileSync(path string, data []byte, mode fs.FileMode, afterTemp func(string)) error {
	tmp := path + tmpSuffix
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale %s: %w", tmp, err)
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	// The umask can narrow the mode at create; set it exactly.
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if afterTemp != nil {
		afterTemp(tmp)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// syncDir makes the directory entries of dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}

// digest is the lower-case hexadecimal SHA-256 of b.
func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fileDigest returns the digest of the file at path, or "" when the file
// does not exist.
func fileDigest(path string) (string, error) {
	b, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return digest(b), nil
}

// exists reports whether path exists. An error other than "not found"
// counts as present, so a caller never treats an unreadable file as gone.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// removeIfPresent removes path and ignores "not found".
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// dirOf is filepath.Dir, named for reading.
func dirOf(path string) string { return filepath.Dir(path) }

// readFile reads a whole file. Tests replace it to inject read errors.
var readFile = os.ReadFile
