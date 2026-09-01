//
// upload.go
// Getting an uploaded file into the data directory without rename(2).
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxUploadBytes bounds one uploaded file. A year of daily roll-ups for a busy
// site is a few megabytes; a hundred is somebody uploading the wrong thing, and
// finding that out after writing it to disk is the expensive way to learn it.
const MaxUploadBytes = 200 << 20

// UploadDir is where uploaded imports are kept, under the data directory. It is
// inside the data directory rather than in the system temporary directory so
// that "back up this one directory" stays true, and so that an import which
// survives a restart still has its file.
const UploadDir = "imports"

// AccountArtifactDir is the durable ownership boundary for one account's
// global import or export files. The account id is a directory component so a
// deletion can erase the complete set without relying on a shard that may
// already have been removed.
func AccountArtifactDir(dataDir, kind string, accountID int64) string {
	return filepath.Join(dataDir, kind, fmt.Sprintf("account-%06d", accountID))
}

// ImportPath is where one import's uploaded file lives. Both owner and import
// ids are in the path, making orphan discovery and account deletion durable.
func ImportPath(dataDir string, accountID, importID int64, filename string) string {
	return filepath.Join(AccountArtifactDir(dataDir, UploadDir, accountID),
		fmt.Sprintf("%06d-%s", importID, SafeFilename(filename)))
}

// SafeFilename reduces an uploaded name to something that cannot escape the
// directory it is written into. A browser will happily send "../../etc/passwd"
// as a filename, and the only safe treatment is to keep the base name and
// throw away everything structural in it.
func SafeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, string(filepath.Separator), "-")

	// A name that is nothing but separators or dots reduces to "." or "..",
	// neither of which is a file we can write.
	if name == "" || name == "." || name == ".." {
		return "upload"
	}

	var cleaned strings.Builder

	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			cleaned.WriteRune(r)
		case r == '.', r == '-', r == '_':
			cleaned.WriteRune(r)
		default:
			cleaned.WriteRune('-')
		}
	}

	if cleaned.Len() > 120 {
		return cleaned.String()[:120]
	}

	return cleaned.String()
}

// SaveUpload writes an uploaded stream to its final path.
//
// It never renames, and that is the entire reason this function exists
// rather than three lines at the call site. rename(2) fails with EXDEV — "cross
// device link" — whenever the source and the destination are on different
// filesystems, which is the normal shape of a Docker bind mount and of a data
// directory on a NAS. An incumbent shipped a rename here and it broke for
// exactly those installs, with an error message that reads like a kernel
// problem rather than a configuration one. Copy, sync, and only then remove the
// source: it works everywhere, and the cost is one pass over a file we were
// going to read anyway.
func SaveUpload(source io.Reader, destination string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return 0, fmt.Errorf("dataio: create upload directory: %w", err)
	}

	file, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("dataio: create %s: %w", destination, err)
	}

	written, err := io.Copy(file, io.LimitReader(source, MaxUploadBytes+1))
	if err != nil {
		return 0, discardUpload(file, destination, fmt.Errorf("dataio: write %s: %w", destination, err))
	}

	if written > MaxUploadBytes {
		return 0, discardUpload(file, destination,
			fmt.Errorf("that file is larger than the %d MB an import may be", MaxUploadBytes>>20))
	}

	// The sync is what makes "the row says the file is there" true after a
	// power cut. An import row pointing at a file that never reached the disk
	// is a job that fails forever with a confusing message.
	if err := file.Sync(); err != nil {
		return 0, discardUpload(file, destination, fmt.Errorf("dataio: sync %s: %w", destination, err))
	}

	if err := file.Close(); err != nil {
		closeErr := fmt.Errorf("dataio: close %s: %w", destination, err)
		if removeErr := os.Remove(destination); removeErr != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("dataio: remove failed upload %s: %w", destination, removeErr))
		}
		return 0, closeErr
	}

	return written, nil
}

// discardUpload closes and removes a partial upload, preserving the triggering
// failure together with any cleanup failures so an operator can act on all of
// the paths that need attention.
func discardUpload(file *os.File, destination string, cause error) error {
	if err := file.Close(); err != nil {
		cause = errors.Join(cause, fmt.Errorf("dataio: close failed upload %s: %w", destination, err))
	}
	if err := os.Remove(destination); err != nil {
		cause = errors.Join(cause, fmt.Errorf("dataio: remove failed upload %s: %w", destination, err))
	}
	return cause
}

// closeResource closes a data import/export resource and joins cleanup failure
// to the operation failure that caused the function to return.
func closeResource(resource io.Closer, err *error, operation string) {
	if closeErr := resource.Close(); closeErr != nil {
		*err = errors.Join(*err, fmt.Errorf("dataio: close %s: %w", operation, closeErr))
	}
}

// MoveFile copies a file to a new path and removes the original, for the same
// reason SaveUpload does not rename: the two paths are routinely on different
// devices, and rename fails there with an error nobody can act on.
func MoveFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("dataio: open %s: %w", source, err)
	}
	if _, err := SaveUpload(in, destination); err != nil {
		if closeErr := in.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("dataio: close %s after copy failure: %w", source, closeErr))
		}
		return err
	}

	if err := in.Close(); err != nil {
		return fmt.Errorf("dataio: close %s: %w", source, err)
	}

	// The copy is durable by the time this runs, so a failure to remove the
	// original leaves a stray file rather than losing the data.
	if err := os.Remove(source); err != nil {
		return fmt.Errorf("dataio: remove %s: %w", source, err)
	}

	return nil
}
