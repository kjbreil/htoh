package scanner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
func ScanOnce(ctx context.Context, cfg config.Config, db *gorm.DB, log *slog.Logger) (int, error) {
	// Aggregate candidates from all source directories
	var allFiles []candidate
	var err error
	for _, sourceDir := range cfg.SourceDirs {
		var files []candidate
		files, err = listCandidates(ctx, cfg.FFprobePath, sourceDir, db, log)
		if err != nil {
			return 0, err
		}
		allFiles = append(allFiles, files...)
	}

	return len(allFiles), nil
}

func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
	}

	// Normalize config paths
	config.NormalizeConfig(&cfg)

	// Initialize database
	db, err := database.InitDB(cfg.WorkDir, cfg.Debug, log)
	if err != nil {
		return fmt.Errorf("init database: %w", err)
	}
	log.Info("Database initialized")

	// Continuous scanning loop
	for {
		// Scan directories
		if len(cfg.SourceDirs) == 1 {
			log.Info("Scanning directory")
		} else {
			log.Info("Scanning directories", slog.Int("count", len(cfg.SourceDirs)))
		}

		// Perform scan
		var fileCount int
		fileCount, err = ScanOnce(ctx, cfg, db, log)
		if err != nil {
			return err
		}

		log.Info("Scan complete", slog.Int("files_found", fileCount))

		// Check if we should scan continuously or exit
		if cfg.ScanInterval == 0 {
			log.Info("Single scan complete, exiting")
			break
		}

		// Wait for next scan
		log.Info("Waiting for next scan", slog.Int("minutes", cfg.ScanInterval))

		select {
		case <-ctx.Done():
			log.Info("Shutdown requested")
			return fmt.Errorf("context cancelled during scan interval: %w", ctx.Err())
		case <-time.After(time.Duration(cfg.ScanInterval) * time.Minute):
			// Continue to next scan
		}
	}

	return nil
}

func listCandidates(ctx context.Context, ffprobePath, root string, db *gorm.DB, log *slog.Logger) ([]candidate, error) {
	var out []candidate
	log.Debug("Walking directory", slog.String("root", root))

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
			log.Debug("Stat error", slog.String("file", p), slog.Any("error", statErr))
			return nil
		}

		existingFile, dbErr := database.GetFileByPath(db, p)
		needsProbe := false

		switch {
		case errors.Is(dbErr, gorm.ErrRecordNotFound):

			needsProbe = true
			log.Debug("New file discovered", slog.String("file", p))
		case dbErr != nil:
			log.Debug("Database error", slog.String("file", p), slog.Any("error", dbErr))
			return nil
		default:

			if existingFile.Size != fileInfo.Size() || !existingFile.ModTime.Equal(fileInfo.ModTime()) {
				needsProbe = true
				log.Debug("File changed", slog.String("file", p))
			} else {
				log.Debug("File unchanged (cached)", slog.String("file", p))
			}
		}

		var probeInfo *runner.ProbeInfoDetailed
		if needsProbe {
			probeInfo, existingFile = handleFileProbe(ctx, ffprobePath, p, fileInfo, existingFile, db, log)
			if probeInfo == nil {
				return nil
			}
		}

		if needsProbe && probeInfo != nil {
			out = addCandidateFromProbe(db, existingFile, probeInfo, p, out, log)
		} else if !needsProbe && existingFile != nil {
			out = addCandidateFromCache(db, existingFile, p, out, log)
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
	log *slog.Logger,
) (*runner.ProbeInfoDetailed, *database.File) {
	ctx2, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	log.Debug("Probing file", slog.String("file", path))

	probeInfo, err := runner.Probe(ctx2, ffprobePath, path)
	if err != nil {
		log.Debug("Ffprobe error", slog.String("file", path), slog.Any("error", err))
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
		log.Debug("Database file update error", slog.String("file", path), slog.Any("error", createErr))
		return nil, existingFile
	}

	// Get the file ID (it was set by CreateOrUpdateFile)
	existingFile, _ = database.GetFileByPath(db, path)

	// Update database with media info
	storeMediaInfo(db, existingFile.ID, probeInfo, log, path)

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
func storeMediaInfo(db *gorm.DB, fileID uint, probeInfo *runner.ProbeInfoDetailed, log *slog.Logger, path string) {
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

	if mediaErr := database.CreateOrUpdateMediaInfo(db, mediaInfo); mediaErr != nil {
		log.Debug("Database media info update error", slog.String("file", path), slog.Any("error", mediaErr))
	}
}

// addCandidateFromProbe adds a file to the candidate list using freshly probed data.
func addCandidateFromProbe(
	db *gorm.DB,
	existingFile *database.File,
	probeInfo *runner.ProbeInfoDetailed,
	path string,
	out []candidate,
	log *slog.Logger,
) []candidate {
	mediaInfo, err := database.GetMediaInfoByFileID(db, existingFile.ID)
	if err != nil {
		return out
	}

	if !database.IsEligibleForConversion(mediaInfo) {
		return out
	}

	// Reset status if file was previously done or failed
	resetFileStatus(db, existingFile, path, log)

	// Convert to old ProbeInfo format for candidate struct
	oldProbeInfo := &runner.ProbeInfo{
		VideoCodec: probeInfo.VideoCodec,
		Width:      probeInfo.Width,
		Height:     probeInfo.Height,
		FPS:        probeInfo.FPS,
		BitRate:    probeInfo.VideoBitrate,
	}

	log.Debug("Queued candidate", slog.String("file", path), slog.String("codec", probeInfo.VideoCodec))

	return append(out, candidate{path: path, info: oldProbeInfo})
}

// addCandidateFromCache adds a file to the candidate list using cached database data.
func addCandidateFromCache(
	db *gorm.DB,
	existingFile *database.File,
	path string,
	out []candidate,
	log *slog.Logger,
) []candidate {
	mediaInfo, err := database.GetMediaInfoByFileID(db, existingFile.ID)
	if err != nil {
		return out
	}

	if !database.IsEligibleForConversion(mediaInfo) {
		return out
	}

	// Reset status if file was previously done or failed
	resetFileStatus(db, existingFile, path, log)

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
func resetFileStatus(db *gorm.DB, file *database.File, path string, log *slog.Logger) {
	if file.Status != "done" && file.Status != "failed" {
		return
	}

	file.Status = "discovered"
	if updateErr := database.CreateOrUpdateFile(db, file); updateErr != nil {
		log.Debug("Failed to reset file status", slog.String("file", path), slog.Any("error", updateErr))
	} else {
		log.Debug("Reset file status to discovered", slog.String("file", path), slog.String("previous_status", "done/failed"))
	}
}
