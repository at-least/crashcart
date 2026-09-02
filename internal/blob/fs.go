package blob

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FS stores objects as files under Dir, one file per key. Local to the
// machine: a second replica does not see this directory, so BLOB_STORE=fs
// is for single-instance deployments (docs/deploy/kubernetes.md).
type FS struct {
	Dir string
}

func (f *FS) path(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	return filepath.Join(f.Dir, filepath.FromSlash(key)), nil
}

// Put writes to a temporary file in the key's directory and renames it
// into place: a reader never sees a partial object, and a crash mid-write
// leaves a stray temporary, not a truncated file at the real name.
func (f *FS) Put(_ context.Context, key string, data []byte) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (f *FS) Get(_ context.Context, key string) ([]byte, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (f *FS) Delete(_ context.Context, key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Ping checks Dir exists and is writable, for a clear error at startup
// rather than at the first upload.
func (f *FS) Ping(context.Context) error {
	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return fmt.Errorf("BLOB_DIR: %w", err)
	}
	tmp, err := os.CreateTemp(f.Dir, ".ping.*")
	if err != nil {
		return fmt.Errorf("BLOB_DIR %s is not writable: %w", f.Dir, err)
	}
	tmp.Close()
	return os.Remove(tmp.Name())
}
