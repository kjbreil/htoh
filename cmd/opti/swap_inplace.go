package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ensureUniqueBackup returns "<src>.original" or timestamped variant.
func ensureUniqueBackup(src string) (string, error) {
	base := src + ".original"
	if _, err := os.Lstat(base); err != nil {
		if os.IsNotExist(err) {
			return base, nil
		}
		return "", err
	}
	ts := time.Now().Format("20060102-150405")
	return base + "." + ts, nil
}

// copyFile copies src -> dst with fsync. Overwrites dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}

	buf := make([]byte, 8<<20) // 8 MiB
	_, copyErr := io.CopyBuffer(out, in, buf)
	syncErr := out.Sync()
	closeErr := out.Close()

	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// SwapInPlaceCopy:
//  1. rename src -> src.original*
//  2. copy newFile -> src (original path/name)
//
// Rollback: if copy fails, rename backup back to src.
func SwapInPlaceCopy(src, newFile string) error {
	dir := filepath.Dir(src)

	backup, err := ensureUniqueBackup(src)
	if err != nil {
		return fmt.Errorf("backup name: %w", err)
	}
	if err := os.Rename(src, backup); err != nil {
		return fmt.Errorf("rename original -> backup: %w", err)
	}
	if err := copyFile(newFile, src); err != nil {
		_ = os.Remove(src)
		_ = os.Rename(backup, src)
		return fmt.Errorf("copy new -> original path failed: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
