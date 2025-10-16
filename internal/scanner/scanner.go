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

type candidate struct {
	path string
	info *runner.ProbeInfo
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
		fmt.Println("Database initialized.")
	}

	// Continuous scanning loop
	for {
		// Scan directories
		if !cfg.Silent {
			if len(cfg.SourceDirs) == 1 {
				fmt.Println("Scanning directory...")
			} else {
				fmt.Printf("Scanning %d directories...\n", len(cfg.SourceDirs))
			}
		}

		// Aggregate candidates from all source directories
		var allFiles []candidate
		for _, sourceDir := range cfg.SourceDirs {
			var files []candidate
			files, err = listCandidates(ctx, cfg.FFprobePath, sourceDir, db, cfg.Debug)
			if err != nil {
				return err
			}
			allFiles = append(allFiles, files...)
		}

		if !cfg.Silent {
			fmt.Printf("Found %d video file(s). Database updated.\n", len(allFiles))
		}

		// Check if we should scan continuously or exit
		if cfg.ScanInterval == 0 {
			if !cfg.Silent {
				fmt.Println("Single scan complete. Exiting.")
			}
			break
		}

		// Wait for next scan
		if !cfg.Silent {
			fmt.Printf("Waiting %d minute(s) before next scan...\n", cfg.ScanInterval)
		}

		select {
		case <-ctx.Done():
			if !cfg.Silent {
				fmt.Println("\nShutdown requested. Exiting.")
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
		fmt.Printf("[scan] Walking %s\n", root)
	}

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}

		// Check file extension
		ext := strings.ToLower(filepath.Ext(d.Name()))
		switch ext {
		case ".mkv", ".mp4", ".mov", ".m4v":
		default:
			return nil
		}

		// Get file info
		fileInfo, err := os.Stat(p)
		if err != nil {
			if debug {
				fmt.Printf("[scan] stat error for %s: %v\n", p, err)
			}
			return nil
		}

		// Check database for existing file
		existingFile, err := database.GetFileByPath(db, p)
		needsProbe := false

		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			// New file
			needsProbe = true
			if debug {
				fmt.Printf("[scan] new file: %s\n", p)
			}
		case err != nil:
			if debug {
				fmt.Printf("[scan] db error for %s: %v\n", p, err)
			}
			return nil
		default:
			// File exists in DB, check if changed
			if existingFile.Size != fileInfo.Size() || !existingFile.ModTime.Equal(fileInfo.ModTime()) {
				needsProbe = true
				if debug {
					fmt.Printf("[scan] file changed: %s\n", p)
				}
			} else if debug {
				fmt.Printf("[scan] file unchanged (cached): %s\n", p)
			}
		}

		var probeInfo *runner.ProbeInfoDetailed
		if needsProbe {
			// Probe the file
			ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			if debug {
				fmt.Printf("[scan] probing %s\n", p)
			}

			var pi *runner.ProbeInfoDetailed
			pi, err = runner.Probe(ctx2, ffprobePath, p)
			if err != nil {
				if debug {
					fmt.Printf("[scan] ffprobe error for %s: %v\n", p, err)
				}
				// Store file with failed status
				file := &database.File{
					Path:    p,
					Name:    filepath.Base(p),
					Size:    fileInfo.Size(),
					ModTime: fileInfo.ModTime(),
					Status:  "failed",
				}
				_ = database.CreateOrUpdateFile(db, file)
				return nil
			}

			probeInfo = pi

			// Update database with file info
			file := &database.File{
				Path:    p,
				Name:    filepath.Base(p),
				Size:    fileInfo.Size(),
				ModTime: fileInfo.ModTime(),
				Status:  "discovered",
			}
			var createErr error
			if createErr = database.CreateOrUpdateFile(db, file); createErr != nil {
				if debug {
					fmt.Printf("[scan] db file update error for %s: %v\n", p, createErr)
				}
				return nil
			}

			// Get the file ID (it was set by CreateOrUpdateFile)
			existingFile, _ = database.GetFileByPath(db, p)

			// Update database with media info
			mediaInfo := &database.MediaInfo{
				FileID:         existingFile.ID,
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
			}
			var mediaErr error
			if mediaErr = database.CreateOrUpdateMediaInfo(db, mediaInfo); mediaErr != nil {
				if debug {
					fmt.Printf("[scan] db media info update error for %s: %v\n", p, mediaErr)
				}
			}
		}

		// Check if file is eligible for conversion and add to candidate list
		if needsProbe && probeInfo != nil {
			// Get MediaInfo for eligibility check
			var mediaInfo *database.MediaInfo
			mediaInfo, err = database.GetMediaInfoByFileID(db, existingFile.ID)
			if err == nil {
				// Check if file is eligible for conversion
				if database.IsEligibleForConversion(mediaInfo) {
					// If file was previously marked as done or failed, reset to discovered
					// This allows re-transcoding when files are replaced or settings change
					if existingFile.Status == "done" || existingFile.Status == "failed" {
						existingFile.Status = "discovered"
						if updateErr := database.CreateOrUpdateFile(db, existingFile); updateErr != nil && debug {
							fmt.Printf("[scan] failed to reset status for %s: %v\n", p, updateErr)
						} else if debug {
							fmt.Printf("[scan] reset status to discovered for %s (was: done/failed, now eligible)\n", p)
						}
					}

					// Convert to old ProbeInfo format for candidate struct
					oldProbeInfo := &runner.ProbeInfo{
						VideoCodec: probeInfo.VideoCodec,
						Width:      probeInfo.Width,
						Height:     probeInfo.Height,
						FPS:        probeInfo.FPS,
						BitRate:    probeInfo.VideoBitrate,
					}
					out = append(out, candidate{path: p, info: oldProbeInfo})
					if debug {
						fmt.Printf("[scan] queued candidate %s (codec=%s)\n", p, probeInfo.VideoCodec)
					}
				}
			}
		} else if !needsProbe && existingFile != nil {
			// Use cached data
			var mediaInfo *database.MediaInfo
			mediaInfo, err = database.GetMediaInfoByFileID(db, existingFile.ID)
			if err == nil {
				// Check if file is eligible for conversion
				if database.IsEligibleForConversion(mediaInfo) {
					// If file was previously marked as done or failed, reset to discovered
					if existingFile.Status == "done" || existingFile.Status == "failed" {
						existingFile.Status = "discovered"
						if updateErr := database.CreateOrUpdateFile(db, existingFile); updateErr != nil && debug {
							fmt.Printf("[scan] failed to reset status for %s: %v\n", p, updateErr)
						} else if debug {
							fmt.Printf("[scan] reset status to discovered for %s (was: done/failed, now eligible)\n", p)
						}
					}

					// Convert to old ProbeInfo format for candidate struct
					oldProbeInfo := &runner.ProbeInfo{
						VideoCodec: mediaInfo.VideoCodec,
						Width:      mediaInfo.Width,
						Height:     mediaInfo.Height,
						FPS:        mediaInfo.FPS,
						BitRate:    mediaInfo.VideoBitrate,
					}
					out = append(out, candidate{path: p, info: oldProbeInfo})
				}
			}
		}

		return nil
	})
	if err != nil {
		return out, fmt.Errorf("failed to walk directory %s: %w", root, err)
	}

	return out, nil
}
