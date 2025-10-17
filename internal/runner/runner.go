package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kjbreil/opti/internal/config"
	"github.com/kjbreil/opti/internal/database"
	"github.com/kjbreil/opti/internal/progress"
)

const (
	ContainerMP4 = "mp4"

	// Quality level thresholds for VBR encoding (CRF values).
	qualityLevelDefault       = 22
	qualityLevelExcellent     = 25
	qualityLevelVeryGood      = 26
	qualityLevelGood          = 27
	qualityLevelFair          = 28
	qualityLevelAcceptable    = 29
	qualityLevelModerate      = 30
	qualityLevelLow           = 31
	qualityLevelVeryLow       = 32
	crfMinimum                = 16
	crfMaximum                = 35
	icqMinimum                = 1
	icqMaximum                = 51
	icqOffset                 = 1
	cqMinimum                 = 0
	cqMaximum                 = 51
	cqOffset                  = 1
	videoToolboxQualityFactor = 2
	videoToolboxQualityMax    = 100

	// Bits per pixel thresholds for quality estimation.
	bppExcellent  = 0.18
	bppVeryGood   = 0.14
	bppGood       = 0.11
	bppFair       = 0.09
	bppAcceptable = 0.075
	bppModerate   = 0.06
	bppLow        = 0.045

	// Bitrate thresholds (bits per second) for quality estimation fallback.
	bitrateExcellent = 20_000_000
	bitrateVeryGood  = 12_000_000
	bitrateGood      = 8_000_000
	bitrateFair      = 5_000_000
	bitrateModerate  = 3_000_000

	// Progress parsing constants.
	microsecondsPerSecond = 1000000.0

	// File and directory permissions.
	workDirPerms = 0o750
)

type QualityChoice struct {
	Mode          string  // "vbr", "cbr", or "cbr_percent"
	TargetBitrate int64   // for CBR mode in bits per second
	CRF           int     // for VBR mode (cpu/vaapi)
	ICQ           int     // for VBR mode (qsv)
	CQ            int     // for VBR mode (nvenc)
	SourceBitrate int64   // source file bitrate for reference
	BitsPerPixel  float64 // bits per pixel for quality estimation
}

type Job struct {
	Src       string
	Rel       string
	Probe     *ProbeInfo
	OutTarget string
	Container string
	Faststart bool
	Quality   QualityChoice
}

// DeriveQualityFromProfile derives quality settings from a quality profile.
// This is the preferred method for quality selection when using profiles.
// Parameters:
//   - profile: The quality profile to use for encoding
//   - info: Media information from the source file (used for adaptive quality in VBR with BitrateMultiplier)
//
// Returns a QualityChoice with appropriate mode (VBR or CBR) and parameters.
func DeriveQualityFromProfile(profile *database.QualityProfile, info *ProbeInfo) QualityChoice {
	if profile == nil {
		// Fallback to default VBR quality if no profile provided
		return DeriveQualityChoice(info, false)
	}

	var bitrate int64
	var bpp float64
	if info != nil {
		bitrate = info.BitRate
		if info.Width > 0 && info.Height > 0 && info.FPS > 0 && bitrate > 0 {
			pixelsPerSecond := float64(info.Width*info.Height) * info.FPS
			if pixelsPerSecond > 0 {
				bpp = float64(bitrate) / pixelsPerSecond
			}
		}
	}

	switch profile.Mode {
	case "cbr":
		// Constant Bitrate mode - use profile's target bitrate
		return QualityChoice{
			Mode:          "cbr",
			TargetBitrate: profile.TargetBitrate,
			CRF:           0,
			ICQ:           0,
			CQ:            0,
			SourceBitrate: bitrate,
			BitsPerPixel:  bpp,
		}

	case "cbr_percent":
		// Constant Bitrate with percentage mode - calculate target from source bitrate
		// Note: This encodes as CBR, but the target is calculated as a percentage of source
		var targetBitrate int64
		if profile.BitrateMultiplier > 0 && bitrate > 0 {
			targetBitrate = int64(float64(bitrate) * profile.BitrateMultiplier)
		} else if profile.TargetBitrate > 0 {
			// Fallback to explicit target if source bitrate unavailable
			targetBitrate = profile.TargetBitrate
		}

		return QualityChoice{
			Mode:          "cbr", // Encode as CBR (percent is just how we calculate target)
			TargetBitrate: targetBitrate,
			CRF:           0,
			ICQ:           0,
			CQ:            0,
			SourceBitrate: bitrate,
			BitsPerPixel:  bpp,
		}

	case "vbr":
		// Variable Bitrate mode
		var targetBitrate int64
		if profile.BitrateMultiplier > 0 && bitrate > 0 {
			// Calculate target from source bitrate using multiplier (e.g., 0.5 = 50% of source)
			targetBitrate = int64(float64(bitrate) * profile.BitrateMultiplier)
		}

		// Determine quality level for VBR mode
		qualityLevel := profile.QualityLevel
		if qualityLevel == 0 {
			// No explicit quality level, use heuristic based on source
			qualityLevel, _ = estimateQualityLevel(info)
		}

		// Convert quality level to engine-specific parameters
		// CRF (16-35): for cpu/vaapi - lower is better quality
		// ICQ (1-51): for qsv - lower is better quality
		// CQ (0-51): for nvenc - lower is better quality
		crf := clampRange(qualityLevel, crfMinimum, crfMaximum)
		icq := clampRange(qualityLevel-icqOffset, icqMinimum, icqMaximum)
		cq := clampRange(qualityLevel-cqOffset, cqMinimum, cqMaximum)

		return QualityChoice{
			Mode:          "vbr",
			TargetBitrate: targetBitrate, // Used for engines that support bitrate hints in VBR mode
			CRF:           crf,           // For cpu/vaapi engines
			ICQ:           icq,           // For qsv engine
			CQ:            cq,            // For nvenc engine
			SourceBitrate: bitrate,       // Reference for logging/stats
			BitsPerPixel:  bpp,           // Reference for logging/stats
		}

	default:
		// Unknown mode, fall back to VBR with heuristic
		return DeriveQualityChoice(info, false)
	}
}

// DeriveQualityChoice derives quality settings using the existing heuristic approach.
// This function is kept for backward compatibility and creates a VBR quality choice.
// For profile-based quality selection, use DeriveQualityFromProfile instead.
func DeriveQualityChoice(info *ProbeInfo, fast bool) QualityChoice {
	base := qualityLevelDefault
	var bpp float64
	var bitrate int64
	if info != nil {
		bitrate = info.BitRate
		base, bpp = estimateQualityLevel(info)
	}
	if fast {
		base++
	}
	crf := clampRange(base, crfMinimum, crfMaximum)
	icq := clampRange(base-icqOffset, icqMinimum, icqMaximum)
	cq := clampRange(base-cqOffset, cqMinimum, cqMaximum)
	return QualityChoice{
		Mode:          "vbr",   // Default to VBR mode for backward compatibility
		TargetBitrate: 0,       // Not used in VBR mode
		CRF:           crf,     // For cpu/vaapi engines
		ICQ:           icq,     // For qsv engine
		CQ:            cq,      // For nvenc engine
		SourceBitrate: bitrate, // Reference for future calculations
		BitsPerPixel:  bpp,     // Reference for quality estimation
	}
}

func estimateQualityLevel(info *ProbeInfo) (int, float64) {
	if info == nil {
		return qualityLevelDefault, 0
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
	case bpp >= bppExcellent:
		return qualityLevelExcellent, bpp
	case bpp >= bppVeryGood:
		return qualityLevelVeryGood, bpp
	case bpp >= bppGood:
		return qualityLevelGood, bpp
	case bpp >= bppFair:
		return qualityLevelFair, bpp
	case bpp >= bppAcceptable:
		return qualityLevelAcceptable, bpp
	case bpp >= bppModerate:
		return qualityLevelModerate, bpp
	case bpp >= bppLow:
		return qualityLevelLow, bpp
	case bpp > 0:
		return qualityLevelVeryLow, bpp
	}
	if bitrate > 0 {
		switch {
		case bitrate >= bitrateExcellent:
			return qualityLevelVeryGood, 0
		case bitrate >= bitrateVeryGood:
			return qualityLevelGood, 0
		case bitrate >= bitrateGood:
			return qualityLevelFair, 0
		case bitrate >= bitrateFair:
			return qualityLevelAcceptable, 0
		case bitrate >= bitrateModerate:
			return qualityLevelModerate, 0
		default:
			return qualityLevelVeryLow, 0
		}
	}
	return qualityLevelModerate, 0
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

// buildCPUArgs builds ffmpeg arguments for CPU (libx265) encoding.
func buildCPUArgs(job Job) []string {
	args := []string{
		"-hide_banner",
		"-v", "error",
		"-progress", "pipe:1",
		"-i", job.Src,
		"-map", "0",
		"-c:v", "libx265",
		"-preset", "medium",
	}

	// Add quality arguments based on mode
	if job.Quality.Mode == "cbr" && job.Quality.TargetBitrate > 0 {
		// CBR mode: use target bitrate
		args = append(args, "-b:v", strconv.FormatInt(job.Quality.TargetBitrate, 10))
	} else {
		// VBR mode: use CRF
		args = append(args, "-crf", strconv.Itoa(job.Quality.CRF))
	}

	// Add stream copy args
	args = append(args, "-c:a", "copy", "-c:s", "copy")
	return args
}

// buildQSVArgs builds ffmpeg arguments for Intel Quick Sync (QSV) encoding.
func buildQSVArgs(job Job) []string {
	args := []string{
		"-hide_banner",
		"-v", "error",
		"-progress", "pipe:1",
		"-hwaccel", "qsv",
		"-c:v", "h264_qsv",
		"-i", job.Src,
		"-map", "0",
		"-c:v", "hevc_qsv",
		"-preset", "veryfast",
	}

	// Add quality arguments based on mode
	if job.Quality.Mode == "cbr" && job.Quality.TargetBitrate > 0 {
		// CBR mode: use target bitrate
		args = append(args, "-b:v", strconv.FormatInt(job.Quality.TargetBitrate, 10))
	} else {
		// VBR mode: use global_quality (ICQ)
		args = append(args, "-global_quality", strconv.Itoa(job.Quality.ICQ))
	}

	// Add stream copy args
	args = append(args, "-c:a", "copy", "-c:s", "copy")
	return args
}

// buildNVENCArgs builds ffmpeg arguments for NVIDIA NVENC encoding.
func buildNVENCArgs(job Job) []string {
	args := []string{
		"-hide_banner",
		"-v", "error",
		"-progress", "pipe:1",
		"-i", job.Src,
		"-map", "0",
		"-c:v", "hevc_nvenc",
		"-preset", "medium",
	}

	// Add quality arguments based on mode
	if job.Quality.Mode == "cbr" && job.Quality.TargetBitrate > 0 {
		// CBR mode: use target bitrate with CBR rate control
		args = append(args,
			"-rc:v", "cbr",
			"-b:v", strconv.FormatInt(job.Quality.TargetBitrate, 10),
		)
	} else {
		// VBR mode: use CQ with VBR rate control
		args = append(args,
			"-rc:v", "vbr",
			"-cq", strconv.Itoa(job.Quality.CQ),
			"-b:v", "0",
		)
	}

	// Add stream copy args
	args = append(args, "-c:a", "copy", "-c:s", "copy")
	return args
}

// buildVAAPIArgs builds ffmpeg arguments for VAAPI (Video Acceleration API) encoding.
// Returns args, vaapiDriverName, and error. The vaapiDriverName should be set as LIBVA_DRIVER_NAME env var.
func buildVAAPIArgs(
	cfg config.Config,
	job Job,
	workerID int,
	prog *progress.Prog,
	log *slog.Logger,
) ([]string, string, error) {
	// Validate and get device (uses default if not specified)
	vaapiDevice, err := validateVAAPIDevice(cfg.VAAPIDevice)
	if err != nil {
		return nil, "", err
	}

	// Update progress with device name
	deviceName := filepath.Base(vaapiDevice)
	prog.Update(workerID, func(r *progress.Row) { r.Device = deviceName })

	// Detect GPU vendor and get appropriate driver name
	var vaapiDriverName string
	vendorID, vendorErr := detectGPUVendor(vaapiDevice)
	if vendorErr != nil && cfg.Debug {
		log.Warn("Could not detect GPU vendor",
			slog.Int("worker_id", workerID),
			slog.Any("error", vendorErr))
	}

	vaapiDriverName = getVAAPIDriverName(vendorID)
	if cfg.Debug {
		vendorName := getVendorName(vendorID)
		if vaapiDriverName != "" {
			log.Info("VAAPI configuration",
				slog.Int("worker_id", workerID),
				slog.String("gpu_vendor", vendorName),
				slog.String("vendor_id", vendorID),
				slog.String("driver", vaapiDriverName),
				slog.String("device", vaapiDevice))
		} else {
			log.Info("VAAPI configuration",
				slog.Int("worker_id", workerID),
				slog.String("gpu_vendor", vendorName),
				slog.String("vendor_id", vendorID),
				slog.String("driver", "auto-detected"),
				slog.String("device", vaapiDevice))
		}
	}

	args := []string{
		"-hide_banner",
		"-v", "error",
		"-progress", "pipe:1",
		"-vaapi_device", vaapiDevice,
		"-i", job.Src,
		"-map", "0",
		"-vf", "format=nv12,hwupload",
		"-c:v", "hevc_vaapi",
	}

	// Add quality arguments based on mode
	if job.Quality.Mode == "cbr" && job.Quality.TargetBitrate > 0 {
		// CBR mode: use target bitrate
		args = append(args, "-b:v", strconv.FormatInt(job.Quality.TargetBitrate, 10))
	} else {
		// VBR mode: use qp (quantization parameter)
		args = append(args, "-qp", strconv.Itoa(job.Quality.CRF))
	}

	// Add stream copy args
	args = append(args, "-c:a", "copy", "-c:s", "copy")
	return args, vaapiDriverName, nil
}

// buildVideoToolboxArgs builds ffmpeg arguments for Apple VideoToolbox encoding.
func buildVideoToolboxArgs(job Job) []string {
	args := []string{
		"-hide_banner",
		"-v", "error",
		"-progress", "pipe:1",
		"-i", job.Src,
		"-map", "0",
		"-c:v", "hevc_videotoolbox",
	}

	// Add quality arguments based on mode
	switch {
	case job.Quality.Mode == "cbr" && job.Quality.TargetBitrate > 0:
		// CBR mode: use target bitrate
		args = append(args, "-b:v", strconv.FormatInt(job.Quality.TargetBitrate, 10))
	case job.Quality.TargetBitrate > 0:
		// VBR mode with bitrate hint: use target bitrate (VideoToolbox handles as bitrate target, not strict CBR)
		// This maintains the existing behavior of using 50% of source bitrate
		args = append(args, "-b:v", strconv.FormatInt(job.Quality.TargetBitrate, 10))
	default:
		// VBR mode with quality value: use -q:v
		// VideoToolbox -q:v range is 0-100 (0 = best quality, 100 = worst)
		qv := job.Quality.CRF * videoToolboxQualityFactor
		if qv > videoToolboxQualityMax {
			qv = videoToolboxQualityMax
		}
		args = append(args, "-q:v", strconv.Itoa(qv))
	}

	// Add stream copy args
	args = append(args, "-c:a", "copy", "-c:s", "copy")
	return args
}

func Transcode(
	ctx context.Context,
	cfg config.Config,
	jb Job,
	workerID int,
	prog *progress.Prog,
	logFunc func(string),
	log *slog.Logger,
) error {
	if err := os.MkdirAll(cfg.WorkDir, workDirPerms); err != nil {
		return fmt.Errorf("failed to create work directory %s: %w", cfg.WorkDir, err)
	}

	// Build ffmpeg arguments based on selected encoding engine
	var args []string
	var vaapiDriverName string // LIBVA_DRIVER_NAME for VAAPI
	var buildErr error

	switch strings.ToLower(cfg.Engine) {
	case "qsv":
		args = buildQSVArgs(jb)
	case "nvenc", "hevc_nvenc":
		args = buildNVENCArgs(jb)
	case "vaapi", "hevc_vaapi":
		args, vaapiDriverName, buildErr = buildVAAPIArgs(cfg, jb, workerID, prog, log)
		if buildErr != nil {
			return buildErr
		}
	case "videotoolbox", "hevc_videotoolbox":
		args = buildVideoToolboxArgs(jb)
	default:
		args = buildCPUArgs(jb)
	}

	if jb.Container == ContainerMP4 {
		if jb.Faststart {
			args = append(args, "-movflags", "+faststart")
		}
		args = append(args, "-f", ContainerMP4)
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

	// Always log the ffmpeg command
	log.Info("FFmpeg command",
		slog.Int("worker_id", workerID),
		slog.String("command", cmdLine))

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
					prog.Update(workerID, func(r *progress.Row) { r.OutTimeS = ms / microsecondsPerSecond })
				case "fps":
					fps, _ := strconv.ParseFloat(val, 64)
					prog.Update(workerID, func(r *progress.Row) { r.FPS = fps })
				case "speed":
					prog.Update(workerID, func(r *progress.Row) { r.Speed = val })
				case "total_size":
					sb, _ := strconv.ParseInt(val, 10, 64)
					prog.Update(workerID, func(r *progress.Row) { r.SizeBytes = sb })
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

// validateVAAPIDevice checks if the specified VAAPI device exists and is accessible.
// If device is empty, it returns the default device (/dev/dri/renderD128) if it exists.
// Returns the device path to use and an error if validation fails.
func validateVAAPIDevice(device string) (string, error) {
	// If no device specified, use default
	if device == "" {
		device = "/dev/dri/renderD128"
	}

	// Check if device exists and is accessible
	_, err := os.Stat(device)
	if err == nil {
		return device, nil
	}

	// Device not accessible - provide helpful error
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("VAAPI device %q not accessible: %w", device, err)
	}

	// Device doesn't exist - list available alternatives
	return "", buildVAAPIDeviceNotFoundError(device)
}

// buildVAAPIDeviceNotFoundError creates an error with available VAAPI devices listed.
func buildVAAPIDeviceNotFoundError(device string) error {
	entries, _ := os.ReadDir("/dev/dri")
	var available []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "renderD") {
			available = append(available, "/dev/dri/"+name)
		}
	}

	if len(available) > 0 {
		return fmt.Errorf(
			"VAAPI device %q not found; available devices: %s",
			device,
			strings.Join(available, ", "),
		)
	}

	return fmt.Errorf("VAAPI device %q not found and no renderD devices found in /dev/dri", device)
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
