package runner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// File and directory permissions for swap operations.
	swapDirPerms = 0o750
)

// SwapInPlaceCopy moves the freshly transcoded file back to the source location,
// renaming the original file to *.original as a backup. If the rename fails due to
// cross-filesystem boundaries, fall back to copy semantics and leave the temp file removed.
// If deleteOriginal is true, the .original backup file is deleted after a successful swap.
func SwapInPlaceCopy(srcPath, newPath string, deleteOriginal bool) error {
	origBackup := srcPath + ".original"
	if err := os.Rename(srcPath, origBackup); err != nil {
		return fmt.Errorf("rename original -> .original failed: %w", err)
	}

	if err := os.Rename(newPath, srcPath); err == nil {
		// Successful rename, delete backup if requested
		if deleteOriginal {
			_ = os.Remove(origBackup)
		}
		return nil
	}

	if err := copyFileContents(newPath, srcPath); err != nil {
		// best-effort rollback; ignore failure if someone already restored it
		_ = os.Rename(origBackup, srcPath)
		return fmt.Errorf("copy new -> original path failed: %w", err)
	}
	_ = os.Remove(newPath)

	// Successful copy, delete backup if requested
	if deleteOriginal {
		_ = os.Remove(origBackup)
	}
	return nil
}

func copyFileContents(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), swapDirPerms); err != nil {
		return fmt.Errorf("failed to create directory for destination: %w", err)
	}
	// #nosec G304 - from path is controlled internally by the application (transcoded output file)
	src, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(to)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer func() { _ = dst.Close() }()

	if _, err = io.Copy(dst, src); err != nil {
		return fmt.Errorf("failed to copy file contents: %w", err)
	}
	if err = dst.Sync(); err != nil {
		return fmt.Errorf("failed to sync destination file: %w", err)
	}
	return nil
}
