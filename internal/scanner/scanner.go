package scanner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kjbreil/opti/internal/config"
	"github.com/kjbreil/opti/internal/database"
	"github.com/kjbreil/opti/internal/runner"
	"gorm.io/gorm"
)

const (
	// Timeout for ffprobe operations.
	probeTimeout = 60 * time.Second
)

type candidate struct {
	path string
	info *runner.ProbeInfo
}

// ScanOnce performs a single scan of all source directories and returns the file count.
// This function checks file sizes/modtimes and runs ffprobe if changes are detected.
func ScanOnce(ctx context.Context, cfg config.Config, db *gorm.DB) (int, error) {
	// Aggregate candidates from all source directories
	var allFiles []candidate
	var err error
	for _, sourceDir := range cfg.SourceDirs {
		var files []candidate
		files, err = listCandidates(ctx, cfg.FFprobePath, sourceDir, db, cfg.Debug)
		if err != nil {
			return 0, err
		}
		allFiles = append(allFiles, files...)
	}

	return len(allFiles), nil
}

func Run(ctx context.Context, cfg config.Config) error {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
	}

	// Normalize config paths
	config.NormalizeConfig(&cfg)

	// Initialize database
	db, err := database.InitDB(cfg.WorkDir, cfg.Debug)
	if err != nil {
		return fmt.Errorf("init database: %w", err)
	}
	if !cfg.Silent {
		fmt.Fprintf(os.Stderr, "Database initialized.\n")
	}

	// Continuous scanning loop
	for {
		// Scan directories
		if !cfg.Silent {
			if len(cfg.SourceDirs) == 1 {
				fmt.Fprintf(os.Stderr, "Scanning directory...\n")
			} else {
				fmt.Fprintf(os.Stderr, "Scanning %d directories...\n", len(cfg.SourceDirs))
			}
		}

		// Perform scan
		var fileCount int
		fileCount, err = ScanOnce(ctx, cfg, db)
		if err != nil {
			return err
		}

		if !cfg.Silent {
			fmt.Fprintf(os.Stderr, "Found %d video file(s). Database updated.\n", fileCount)
		}

		// Check if we should scan continuously or exit
		if cfg.ScanInterval == 0 {
			if !cfg.Silent {
				fmt.Fprintf(os.Stderr, "Single scan complete. Exiting.\n")
			}
			break
		}

		// Wait for next scan
		if !cfg.Silent {
			fmt.Fprintf(os.Stderr, "Waiting %d minute(s) before next scan...\n", cfg.ScanInterval)
		}

		select {
		case <-ctx.Done():
			if !cfg.Silent {
				fmt.Fprintf(os.Stderr, "\nShutdown requested. Exiting.\n")
			}
			return fmt.Errorf("context cancelled during scan interval: %w", ctx.Err())
		case <-time.After(time.Duration(cfg.ScanInterval) * time.Minute):
			// Continue to next scan
		}
	}

	return nil
}

func listCandidates(ctx context.Context, ffprobePath, root string, db *gorm.DB, debug bool) ([]candidate, error) {
	var out []candidate
	if debug {
		fmt.Fprintf(os.Stderr, "[scan] Walking %s\n", root)
	}

	var err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		switch ext {
		case ".mkv", ".mp4", ".mov", ".m4v":
		default:
			return nil
		}

		fileInfo, statErr := os.Stat(p)
		if statErr != nil {
			if debug {
				fmt.Fprintf(os.Stderr, "[scan] stat error for %s: %v\n", p, statErr)
			}
			return nil
		}

		existingFile, dbErr := database.GetFileByPath(db, p)
		needsProbe := false

		switch {
		case errors.Is(dbErr, gorm.ErrRecordNotFound):

			needsProbe = true
			if debug {
				fmt.Fprintf(os.Stderr, "[scan] new file: %s\n", p)
			}
		case dbErr != nil:
			if debug {
				fmt.Fprintf(os.Stderr, "[scan] db error for %s: %v\n", p, dbErr)
			}
			return nil
		default:

			if existingFile.Size != fileInfo.Size() || !existingFile.ModTime.Equal(fileInfo.ModTime()) {
				needsProbe = true
				if debug {
					fmt.Fprintf(os.Stderr, "[scan] file changed: %s\n", p)
				}
			} else if debug {
				fmt.Fprintf(os.Stderr, "[scan] file unchanged (cached): %s\n", p)
			}
		}

		var probeInfo *runner.ProbeInfoDetailed
		if needsProbe {
			probeInfo, existingFile = handleFileProbe(ctx, ffprobePath, p, fileInfo, existingFile, db, debug)
			if probeInfo == nil {
				return nil
			}
		}

		if needsProbe && probeInfo != nil {
			out = addCandidateFromProbe(db, existingFile, probeInfo, p, out, debug)
		} else if !needsProbe && existingFile != nil {
			out = addCandidateFromCache(db, existingFile, p, out, debug)
		}

		return nil
	})
	if err != nil {
		return out, fmt.Errorf("failed to walk directory %s: %w", root, err)
	}

	return out, nil
}

// handleFileProbe probes a file and updates database with file and media info.
// Returns probeInfo and existingFile if successful, nil probeInfo on error.
func handleFileProbe(
	ctx context.Context,
	ffprobePath, path string,
	fileInfo os.FileInfo,
	existingFile *database.File,
	db *gorm.DB,
	debug bool,
) (*runner.ProbeInfoDetailed, *database.File) {
	ctx2, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	if debug {
		fmt.Fprintf(os.Stderr, "[scan] probing %s\n", path)
	}

	probeInfo, err := runner.Probe(ctx2, ffprobePath, path)
	if err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[scan] ffprobe error for %s: %v\n", path, err)
		}
		storeFailedFile(db, path, fileInfo)
		return nil, existingFile
	}

	// Update database with file info
	file := &database.File{
		ID:               0, // Will be auto-assigned by GORM
		Path:             path,
		Name:             filepath.Base(path),
		Size:             fileInfo.Size(),
		ModTime:          fileInfo.ModTime(),
		Status:           "discovered",
		QualityProfileID: 0, // Will use default when added to queue
		QualityProfile:   nil,
		CreatedAt:        time.Time{}, // Will be auto-set by GORM
		UpdatedAt:        time.Time{}, // Will be auto-set by GORM
	}

	if createErr := database.CreateOrUpdateFile(db, file); createErr != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[scan] db file update error for %s: %v\n", path, createErr)
		}
		return nil, existingFile
	}

	// Get the file ID (it was set by CreateOrUpdateFile)
	existingFile, _ = database.GetFileByPath(db, path)

	// Update database with media info
	storeMediaInfo(db, existingFile.ID, probeInfo, debug, path)

	return probeInfo, existingFile
}

// storeFailedFile stores a file with failed status in the database.
func storeFailedFile(db *gorm.DB, path string, fileInfo os.FileInfo) {
	file := &database.File{
		ID:               0, // Will be auto-assigned by GORM
		Path:             path,
		Name:             filepath.Base(path),
		Size:             fileInfo.Size(),
		ModTime:          fileInfo.ModTime(),
		Status:           "failed",
		QualityProfileID: 0,
		QualityProfile:   nil,
		CreatedAt:        time.Time{}, // Will be auto-set by GORM
		UpdatedAt:        time.Time{}, // Will be auto-set by GORM
	}
	_ = database.CreateOrUpdateFile(db, file)
}

// storeMediaInfo stores media information in the database.
func storeMediaInfo(db *gorm.DB, fileID uint, probeInfo *runner.ProbeInfoDetailed, debug bool, path string) {
	mediaInfo := &database.MediaInfo{
		ID:             0, // Will be auto-assigned by GORM
		FileID:         fileID,
		File:           nil, // Will be loaded via Preload when needed
		Duration:       probeInfo.Duration,
		FormatBitrate:  probeInfo.FormatBitrate,
		Container:      probeInfo.Container,
		VideoCodec:     probeInfo.VideoCodec,
		VideoProfile:   probeInfo.VideoProfile,
		Width:          probeInfo.Width,
		Height:         probeInfo.Height,
		CodedWidth:     probeInfo.CodedWidth,
		CodedHeight:    probeInfo.CodedHeight,
		FPS:            probeInfo.FPS,
		AspectRatio:    probeInfo.AspectRatio,
		VideoBitrate:   probeInfo.VideoBitrate,
		PixelFormat:    probeInfo.PixelFormat,
		BitDepth:       probeInfo.BitDepth,
		ChromaLocation: probeInfo.ChromaLocation,
		ColorSpace:     probeInfo.ColorSpace,
		ColorRange:     probeInfo.ColorRange,
		ColorPrimaries: probeInfo.ColorPrimaries,
		CreatedAt:      time.Time{}, // Will be auto-set by GORM
		UpdatedAt:      time.Time{}, // Will be auto-set by GORM
	}

	if mediaErr := database.CreateOrUpdateMediaInfo(db, mediaInfo); mediaErr != nil && debug {
		fmt.Fprintf(os.Stderr, "[scan] db media info update error for %s: %v\n", path, mediaErr)
	}
}

// addCandidateFromProbe adds a file to the candidate list using freshly probed data.
func addCandidateFromProbe(
	db *gorm.DB,
	existingFile *database.File,
	probeInfo *runner.ProbeInfoDetailed,
	path string,
	out []candidate,
	debug bool,
) []candidate {
	mediaInfo, err := database.GetMediaInfoByFileID(db, existingFile.ID)
	if err != nil {
		return out
	}

	if !database.IsEligibleForConversion(mediaInfo) {
		return out
	}

	// Reset status if file was previously done or failed
	resetFileStatus(db, existingFile, path, debug)

	// Convert to old ProbeInfo format for candidate struct
	oldProbeInfo := &runner.ProbeInfo{
		VideoCodec: probeInfo.VideoCodec,
		Width:      probeInfo.Width,
		Height:     probeInfo.Height,
		FPS:        probeInfo.FPS,
		BitRate:    probeInfo.VideoBitrate,
	}

	if debug {
		fmt.Fprintf(os.Stderr, "[scan] queued candidate %s (codec=%s)\n", path, probeInfo.VideoCodec)
	}

	return append(out, candidate{path: path, info: oldProbeInfo})
}

// addCandidateFromCache adds a file to the candidate list using cached database data.
func addCandidateFromCache(
	db *gorm.DB,
	existingFile *database.File,
	path string,
	out []candidate,
	debug bool,
) []candidate {
	mediaInfo, err := database.GetMediaInfoByFileID(db, existingFile.ID)
	if err != nil {
		return out
	}

	if !database.IsEligibleForConversion(mediaInfo) {
		return out
	}

	// Reset status if file was previously done or failed
	resetFileStatus(db, existingFile, path, debug)

	// Convert to old ProbeInfo format for candidate struct
	oldProbeInfo := &runner.ProbeInfo{
		VideoCodec: mediaInfo.VideoCodec,
		Width:      mediaInfo.Width,
		Height:     mediaInfo.Height,
		FPS:        mediaInfo.FPS,
		BitRate:    mediaInfo.VideoBitrate,
	}

	return append(out, candidate{path: path, info: oldProbeInfo})
}

// resetFileStatus resets a file's status to "discovered" if it was previously done or failed.
func resetFileStatus(db *gorm.DB, file *database.File, path string, debug bool) {
	if file.Status != "done" && file.Status != "failed" {
		return
	}

	file.Status = "discovered"
	if updateErr := database.CreateOrUpdateFile(db, file); updateErr != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[scan] failed to reset status for %s: %v\n", path, updateErr)
		}
	} else if debug {
		fmt.Fprintf(os.Stderr, "[scan] reset status to discovered for %s (was: done/failed, now eligible)\n", path)
	}
}
