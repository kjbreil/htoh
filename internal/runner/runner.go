package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	containerMP4 = "mp4"
)

type Config struct {
	SourceDirs   []string
	WorkDir      string
	Interactive  bool
	Keep         bool
	Silent       bool
	Workers      int
	ScanInterval int    // minutes between scans, 0 = scan once and exit
	Engine       string // cpu|qsv|nvenc|vaapi
	VAAPIDevice  string // Hardware device path for VAAPI (e.g., /dev/dri/renderD128)
	FFmpegPath   string
	FFprobePath  string
	Debug        bool
	ForceMP4     bool
	FaststartMP4 bool
	FastMode     bool
}

type qualityChoice struct {
	CRF           int
	ICQ           int
	CQ            int
	SourceBitrate int64
	BitsPerPixel  float64
}

type job struct {
	Src       string
	Rel       string
	Probe     *ProbeInfo
	OutTarget string
	Container string
	Faststart bool
	Quality   qualityChoice
}

// NormalizeConfig sets default values for config fields that may be empty.
func NormalizeConfig(cfg *Config) {
	// Set default ffmpeg path if not provided
	ffmpegPath := strings.TrimSpace(cfg.FFmpegPath)
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	cfg.FFmpegPath = ffmpegPath

	// Set default ffprobe path if not provided
	ffprobePath := strings.TrimSpace(cfg.FFprobePath)
	if ffprobePath == "" {
		ffprobePath = "ffprobe"
		if p := findSibling(cfg.FFmpegPath, "ffprobe"); p != "" {
			ffprobePath = p
		}
	}
	cfg.FFprobePath = ffprobePath

	if cfg.Debug {
		fmt.Printf("Using ffmpeg: %s\n", cfg.FFmpegPath)
		fmt.Printf("Using ffprobe: %s\n", cfg.FFprobePath)
	}
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
	}

	// Normalize config paths
	NormalizeConfig(&cfg)

	// Initialize database
	db, err := InitDB(cfg.WorkDir, cfg.Debug)
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

type candidate struct {
	path string
	info *ProbeInfo
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
		existingFile, err := GetFileByPath(db, p)
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

		var probeInfo *ProbeInfoDetailed
		if needsProbe {
			// Probe the file
			ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			if debug {
				fmt.Printf("[scan] probing %s\n", p)
			}

			var pi *ProbeInfoDetailed
			pi, err = probe(ctx2, ffprobePath, p)
			if err != nil {
				if debug {
					fmt.Printf("[scan] ffprobe error for %s: %v\n", p, err)
				}
				// Store file with failed status
				file := &File{
					Path:    p,
					Name:    filepath.Base(p),
					Size:    fileInfo.Size(),
					ModTime: fileInfo.ModTime(),
					Status:  "failed",
				}
				_ = CreateOrUpdateFile(db, file)
				return nil
			}

			probeInfo = pi

			// Update database with file info
			file := &File{
				Path:    p,
				Name:    filepath.Base(p),
				Size:    fileInfo.Size(),
				ModTime: fileInfo.ModTime(),
				Status:  "discovered",
			}
			var createErr error
			if createErr = CreateOrUpdateFile(db, file); createErr != nil {
				if debug {
					fmt.Printf("[scan] db file update error for %s: %v\n", p, createErr)
				}
				return nil
			}

			// Get the file ID (it was set by CreateOrUpdateFile)
			existingFile, _ = GetFileByPath(db, p)

			// Update database with media info
			mediaInfo := &MediaInfo{
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
			if mediaErr = CreateOrUpdateMediaInfo(db, mediaInfo); mediaErr != nil {
				if debug {
					fmt.Printf("[scan] db media info update error for %s: %v\n", p, mediaErr)
				}
			}
		}

		// Check if file is eligible for conversion and add to candidate list
		if needsProbe && probeInfo != nil {
			// Get MediaInfo for eligibility check
			var mediaInfo *MediaInfo
			mediaInfo, err = GetMediaInfoByFileID(db, existingFile.ID)
			if err == nil {
				// Check if file is eligible for conversion
				if IsEligibleForConversion(mediaInfo) {
					// If file was previously marked as done or failed, reset to discovered
					// This allows re-transcoding when files are replaced or settings change
					if existingFile.Status == "done" || existingFile.Status == "failed" {
						existingFile.Status = "discovered"
						if updateErr := CreateOrUpdateFile(db, existingFile); updateErr != nil && debug {
							fmt.Printf("[scan] failed to reset status for %s: %v\n", p, updateErr)
						} else if debug {
							fmt.Printf("[scan] reset status to discovered for %s (was: done/failed, now eligible)\n", p)
						}
					}

					// Convert to old ProbeInfo format for candidate struct
					oldProbeInfo := &ProbeInfo{
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
			var mediaInfo *MediaInfo
			mediaInfo, err = GetMediaInfoByFileID(db, existingFile.ID)
			if err == nil {
				// Check if file is eligible for conversion
				if IsEligibleForConversion(mediaInfo) {
					// If file was previously marked as done or failed, reset to discovered
					if existingFile.Status == "done" || existingFile.Status == "failed" {
						existingFile.Status = "discovered"
						if updateErr := CreateOrUpdateFile(db, existingFile); updateErr != nil && debug {
							fmt.Printf("[scan] failed to reset status for %s: %v\n", p, updateErr)
						} else if debug {
							fmt.Printf("[scan] reset status to discovered for %s (was: done/failed, now eligible)\n", p)
						}
					}

					// Convert to old ProbeInfo format for candidate struct
					oldProbeInfo := &ProbeInfo{
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

func deriveQualityChoice(info *ProbeInfo, fast bool) qualityChoice {
	base := 22
	var bpp float64
	var bitrate int64
	if info != nil {
		bitrate = info.BitRate
		base, bpp = estimateQualityLevel(info)
	}
	if fast {
		base++
	}
	crf := clampRange(base, 16, 35)
	icq := clampRange(base-1, 1, 51)
	cq := clampRange(base-1, 0, 51)
	return qualityChoice{
		CRF:           crf,
		ICQ:           icq,
		CQ:            cq,
		SourceBitrate: bitrate,
		BitsPerPixel:  bpp,
	}
}

func estimateQualityLevel(info *ProbeInfo) (int, float64) {
	if info == nil {
		return 22, 0
	}
	w, h := info.Width, info.Height
	fps := info.FPS
	bitrate := info.BitRate
	var bpp float64
	if w > 0 && h > 0 && fps > 0 && bitrate > 0 {
		pixelsPerSecond := float64(w*h) * fps
		if pixelsPerSecond > 0 {
			bpp = float64(bitrate) / pixelsPerSecond
		}
	}
	switch {
	case bpp >= 0.18:
		return 25, bpp
	case bpp >= 0.14:
		return 26, bpp
	case bpp >= 0.11:
		return 27, bpp
	case bpp >= 0.09:
		return 28, bpp
	case bpp >= 0.075:
		return 29, bpp
	case bpp >= 0.06:
		return 30, bpp
	case bpp >= 0.045:
		return 31, bpp
	case bpp > 0:
		return 32, bpp
	}
	if bitrate > 0 {
		switch {
		case bitrate >= 20_000_000:
			return 26, 0
		case bitrate >= 12_000_000:
			return 27, 0
		case bitrate >= 8_000_000:
			return 28, 0
		case bitrate >= 5_000_000:
			return 29, 0
		case bitrate >= 3_000_000:
			return 30, 0
		default:
			return 32, 0
		}
	}
	return 30, 0
}

func clampRange(val, minVal, maxVal int) int {
	if val < minVal {
		return minVal
	}
	if val > maxVal {
		return maxVal
	}
	return val
}

func transcode(ctx context.Context, cfg Config, jb job, workerID int, prog *Prog, logFunc func(string)) error {
	if err := os.MkdirAll(cfg.WorkDir, 0o750); err != nil {
		return fmt.Errorf("failed to create work directory %s: %w", cfg.WorkDir, err)
	}

	// build args with -progress pipe:1 and quiet errors
	var args []string
	var vaapiDriverName string // LIBVA_DRIVER_NAME for VAAPI
	switch strings.ToLower(cfg.Engine) {
	case "qsv":
		args = []string{
			"-hide_banner",
			"-v", "error",
			"-progress", "pipe:1",
			// input
			"-hwaccel", "qsv",
			"-c:v", "h264_qsv",
			"-i", jb.Src,
			// mapping
			"-map", "0",
			// video
			"-c:v", "hevc_qsv",
			"-preset", "veryfast",
		}
		args = append(args,
			"-global_quality", strconv.Itoa(jb.Quality.ICQ),
		)
		args = append(args,
			"-c:a", "copy",
			"-c:s", "copy",
		)
	case "nvenc", "hevc_nvenc":
		args = []string{
			"-hide_banner",
			"-v", "error",
			"-progress", "pipe:1",
			"-i", jb.Src,
			"-map", "0",
			"-c:v", "hevc_nvenc",
			"-preset", "medium",
		}
		args = append(args,
			"-rc:v", "vbr",
			"-cq", strconv.Itoa(jb.Quality.CQ),
			"-b:v", "0",
		)
		args = append(args,
			"-c:a", "copy",
			"-c:s", "copy",
		)
	case "vaapi", "hevc_vaapi":
		// Validate and get device (uses default if not specified)
		vaapiDevice, err := validateVAAPIDevice(cfg.VAAPIDevice)
		if err != nil {
			return err
		}

		// Update progress with device name
		deviceName := filepath.Base(vaapiDevice)
		prog.Update(workerID, func(r *Row) { r.Device = deviceName })

		// Detect GPU vendor and get appropriate driver name
		vendorID, err := detectGPUVendor(vaapiDevice)
		if err != nil && cfg.Debug {
			fmt.Fprintf(os.Stderr, "[worker %d] Warning: Could not detect GPU vendor: %v\n", workerID, err)
		}

		vaapiDriverName = getVAAPIDriverName(vendorID)
		if cfg.Debug {
			vendorName := getVendorName(vendorID)
			if vaapiDriverName != "" {
				fmt.Fprintf(os.Stderr, "[worker %d] VAAPI: Using %s GPU (%s) with driver %s on device %s\n",
					workerID, vendorName, vendorID, vaapiDriverName, vaapiDevice)
			} else {
				fmt.Fprintf(os.Stderr, "[worker %d] VAAPI: Using %s GPU (%s) with auto-detected driver on device %s\n",
					workerID, vendorName, vendorID, vaapiDevice)
			}
		}

		args = []string{
			"-hide_banner",
			"-v", "error",
			"-progress", "pipe:1",
			"-vaapi_device", vaapiDevice,
			"-i", jb.Src,
			"-map", "0",
			"-vf", "format=nv12,hwupload",
			"-c:v", "hevc_vaapi",
		}
		args = append(args,
			"-qp", strconv.Itoa(jb.Quality.CRF),
		)
		args = append(args,
			"-c:a", "copy",
			"-c:s", "copy",
		)
	case "videotoolbox", "hevc_videotoolbox":
		args = []string{
			"-hide_banner",
			"-v", "error",
			"-progress", "pipe:1",
			"-i", jb.Src,
			"-map", "0",
			"-c:v", "hevc_videotoolbox",
		}
		// Use 50% of source bitrate to achieve file size reduction with H.265
		// H.265 is ~2x more efficient than H.264, so encoding at 50% bitrate
		// maintains similar visual quality while reducing file size by ~50%
		if jb.Quality.SourceBitrate > 0 {
			targetBitrate := jb.Quality.SourceBitrate / 2
			args = append(args,
				"-b:v", strconv.FormatInt(targetBitrate, 10),
			)
		} else {
			// Fallback to quality mode if source bitrate is unknown
			// VideoToolbox -q:v range is 0-100 (0 = best quality, 100 = worst)
			qv := jb.Quality.CRF * 2
			if qv > 100 {
				qv = 100
			}
			args = append(args,
				"-q:v", strconv.Itoa(qv),
			)
		}
		args = append(args,
			"-c:a", "copy",
			"-c:s", "copy",
		)
	default:
		args = []string{
			"-hide_banner",
			"-v", "error",
			"-progress", "pipe:1",
			"-i", jb.Src,
			"-map", "0",
			"-c:v", "libx265",
			"-preset", "medium",
		}
		args = append(args,
			"-crf", strconv.Itoa(jb.Quality.CRF),
		)
		args = append(args,
			"-c:a", "copy",
			"-c:s", "copy",
		)
	}

	if jb.Container == containerMP4 {
		if jb.Faststart {
			args = append(args, "-movflags", "+faststart")
		}
		args = append(args, "-f", containerMP4)
	}
	args = append(args, jb.OutTarget)

	// Build command line string for logging
	cmdLine := cfg.FFmpegPath
	for _, arg := range args {
		// Quote arguments with spaces
		if strings.Contains(arg, " ") {
			cmdLine += " \"" + arg + "\""
		} else {
			cmdLine += " " + arg
		}
	}

	// Always log the ffmpeg command to stderr
	fmt.Fprintf(os.Stderr, "[worker %d] ffmpeg command: %s\n", workerID, cmdLine)

	// Also log via callback if provided (for queue logs)
	if logFunc != nil {
		logFunc(fmt.Sprintf("FFmpeg command: %s", cmdLine))
	}

	// Log the command path being used (for debugging "exec: no command" errors)
	if cfg.FFmpegPath == "" {
		return errors.New("ffmpeg path is empty - this should not happen")
	}

	// #nosec G204 - FFmpegPath is validated during config initialization and comes from config file or defaults to "ffmpeg"
	cmd := exec.CommandContext(ctx, cfg.FFmpegPath, args...)

	// Set LIBVA_DRIVER_NAME environment variable for VAAPI if needed
	if vaapiDriverName != "" {
		cmd.Env = append(os.Environ(), "LIBVA_DRIVER_NAME="+vaapiDriverName)
	}

	var stdout io.ReadCloser
	var pipeErr error
	stdout, pipeErr = cmd.StdoutPipe()
	if pipeErr != nil {
		return fmt.Errorf("failed to create stdout pipe for ffmpeg: %w", pipeErr)
	}
	// keep stderr quiet; if needed, attach to os.Stderr
	cmd.Stderr = io.Discard

	var startErr error
	if startErr = cmd.Start(); startErr != nil {
		return fmt.Errorf("failed to start ffmpeg process: %w", startErr)
	}

	// Parse -progress lines
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			// key=value
			if i := strings.IndexByte(line, '='); i > 0 {
				key := line[:i]
				val := line[i+1:]
				switch key {
				case "out_time_ms":
					ms, _ := strconv.ParseFloat(val, 64)
					prog.Update(workerID, func(r *Row) { r.OutTimeS = ms / 1000000.0 })
				case "fps":
					fps, _ := strconv.ParseFloat(val, 64)
					prog.Update(workerID, func(r *Row) { r.FPS = fps })
				case "speed":
					prog.Update(workerID, func(r *Row) { r.Speed = val })
				case "total_size":
					sb, _ := strconv.ParseInt(val, 10, 64)
					prog.Update(workerID, func(r *Row) { r.SizeBytes = sb })
				case "progress":
					// When val == "end", ffmpeg will exit shortly; no action needed
				}
			}
		}
		done <- sc.Err()
	}()

	waitErr := cmd.Wait()
	readErr := <-done
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return fmt.Errorf("ffmpeg process failed: %w", waitErr)
	}
	// Always swap in-place and delete the original backup
	var swapErr error
	if swapErr = SwapInPlaceCopy(jb.Src, jb.OutTarget, true); swapErr != nil {
		return swapErr
	}
	return nil
}

func findSibling(ffmpeg, name string) string {
	if ffmpeg == "" {
		return ""
	}
	dir := filepath.Dir(ffmpeg)
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// validateVAAPIDevice checks if the specified VAAPI device exists and is accessible.
// If device is empty, it returns the default device (/dev/dri/renderD128) if it exists.
// Returns the device path to use and an error if validation fails.
func validateVAAPIDevice(device string) (string, error) {
	// If no device specified, use default
	if device == "" {
		device = "/dev/dri/renderD128"
	}

	// Check if device exists and is accessible
	if _, err := os.Stat(device); err != nil {
		if os.IsNotExist(err) {
			// Try to list available devices for helpful error message
			entries, _ := os.ReadDir("/dev/dri")
			var available []string
			for _, e := range entries {
				name := e.Name()
				if strings.HasPrefix(name, "renderD") {
					available = append(available, "/dev/dri/"+name)
				}
			}
			if len(available) > 0 {
				return "", fmt.Errorf(
					"VAAPI device %q not found; available devices: %s",
					device,
					strings.Join(available, ", "),
				)
			}
			return "", fmt.Errorf("VAAPI device %q not found and no renderD devices found in /dev/dri", device)
		}
		return "", fmt.Errorf("VAAPI device %q not accessible: %w", device, err)
	}

	return device, nil
}

// detectGPUVendor reads the GPU vendor ID from sysfs for the given VAAPI device.
// Returns the vendor ID (e.g., "0x8086" for Intel, "0x1002" for AMD) or an error.
func detectGPUVendor(device string) (string, error) {
	// Extract device name from path (e.g., "renderD128" from "/dev/dri/renderD128")
	devName := filepath.Base(device)

	// Read vendor ID from sysfs
	vendorPath := filepath.Join("/sys/class/drm", devName, "device/vendor")
	// #nosec G304 - vendorPath is constructed from system device paths under /sys/class/drm
	vendorBytes, err := os.ReadFile(vendorPath)
	if err != nil {
		return "", fmt.Errorf("failed to read GPU vendor from %s: %w", vendorPath, err)
	}

	// Trim whitespace and return
	vendorID := strings.TrimSpace(string(vendorBytes))
	return vendorID, nil
}

// getVAAPIDriverName returns the appropriate LIBVA_DRIVER_NAME for the given GPU vendor ID.
// Returns empty string if vendor is unknown (system will auto-detect).
func getVAAPIDriverName(vendorID string) string {
	switch strings.ToLower(vendorID) {
	case "0x8086":
		// Intel: use iHD driver (modern driver for Gen 8+)
		return "iHD"
	case "0x1002":
		// AMD: use radeonsi driver
		return "radeonsi"
	default:
		// Unknown vendor, return empty to let system auto-detect
		return ""
	}
}

// getVendorName returns a human-readable name for the GPU vendor ID.
func getVendorName(vendorID string) string {
	switch strings.ToLower(vendorID) {
	case "0x8086":
		return "Intel"
	case "0x1002":
		return "AMD"
	case "0x10de":
		return "NVIDIA"
	default:
		return "Unknown"
	}
}
