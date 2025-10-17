package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config represents the runtime configuration for the opti application.
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
		fmt.Fprintf(os.Stderr, "Using ffmpeg: %s\n", cfg.FFmpegPath)
		fmt.Fprintf(os.Stderr, "Using ffprobe: %s\n", cfg.FFprobePath)
	}
}

// findSibling returns the path to a sibling binary in the same directory as the given binary.
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

// TomlConfig represents the TOML configuration file structure.
// All fields use snake_case TOML tags to match common configuration conventions.
type TomlConfig struct {
	SourceDir    []string `toml:"source_dir"`
	SourceDirs   []string `toml:"source_dirs"`
	WorkDir      string   `toml:"work_dir"`
	Interactive  bool     `toml:"interactive"`
	Keep         bool     `toml:"keep"`
	Silent       bool     `toml:"silent"`
	Workers      int      `toml:"workers"`
	ScanInterval int      `toml:"scan_interval"`
	Engine       string   `toml:"engine"`
	VAAPIDevice  string   `toml:"vaapi_device"`
	FFmpegPath   string   `toml:"ffmpeg_path"`
	FFprobePath  string   `toml:"ffprobe_path"`
	Debug        bool     `toml:"debug"`
	ForceMP4     bool     `toml:"force_mp4"`
	FaststartMP4 bool     `toml:"faststart_mp4"`
	FastMode     bool     `toml:"fast_mode"`
}

// LoadFromFile reads and parses a TOML configuration file.
// It returns a Config populated with values from the file.
// Returns an error if the file cannot be read or parsed.
func LoadFromFile(path string) (*Config, error) {
	// Check if file exists
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found: %s", path)
		}
		return nil, fmt.Errorf("cannot access config file: %w", err)
	}

	// Parse TOML file
	var tomlCfg TomlConfig
	if _, err := toml.DecodeFile(path, &tomlCfg); err != nil {
		return nil, fmt.Errorf("failed to parse TOML config: %w", err)
	}

	// Merge source_dir and source_dirs into SourceDirs slice
	var sourceDirs []string
	sourceDirs = append(sourceDirs, tomlCfg.SourceDir...)
	sourceDirs = append(sourceDirs, tomlCfg.SourceDirs...)

	// Convert to Config
	cfg := &Config{
		SourceDirs:   sourceDirs,
		WorkDir:      tomlCfg.WorkDir,
		Interactive:  tomlCfg.Interactive,
		Keep:         tomlCfg.Keep,
		Silent:       tomlCfg.Silent,
		Workers:      tomlCfg.Workers,
		ScanInterval: tomlCfg.ScanInterval,
		Engine:       tomlCfg.Engine,
		VAAPIDevice:  tomlCfg.VAAPIDevice,
		FFmpegPath:   tomlCfg.FFmpegPath,
		FFprobePath:  tomlCfg.FFprobePath,
		Debug:        tomlCfg.Debug,
		ForceMP4:     tomlCfg.ForceMP4,
		FaststartMP4: tomlCfg.FaststartMP4,
		FastMode:     tomlCfg.FastMode,
	}

	return cfg, nil
}

// GenerateDefault creates an example configuration file with all available options.
// The file includes helpful comments explaining each setting and shows sensible defaults.
// Returns an error if the file cannot be written.
func GenerateDefault(path string) error {
	content := `# Opti Configuration File
#
# This is an example configuration file for the Opti H.264 to HEVC transcoder.
# All settings shown here are optional - CLI flags will override config file values.
#
# To use this config: opti --config /path/to/config.toml

# ============================================================================
# PATHS
# ============================================================================

# source_dir: Single directory containing video files to transcode
# Default: current directory
# source_dir = "/path/to/videos"

# source_dirs: Multiple directories to scan for video files
# You can specify either source_dir OR source_dirs (or both - they will be merged)
# source_dirs = [
#     "/path/to/videos1",
#     "/path/to/videos2",
#     "/mnt/media/movies"
# ]

# work_dir: Directory where transcoded files will be saved
# Default: current directory
# work_dir = "/path/to/output"

# ffmpeg_path: Path to ffmpeg binary
# Default: "ffmpeg" (searches in PATH)
# ffmpeg_path = "/usr/bin/ffmpeg"

# ffprobe_path: Path to ffprobe binary
# Default: auto-detected from ffmpeg location or "ffprobe"
# ffprobe_path = "/usr/bin/ffprobe"

# ============================================================================
# ENCODING ENGINE
# ============================================================================

# engine: Video encoding engine to use
# Options:
#   - "cpu"         : Software encoding with libx265 (slowest, best compatibility)
#   - "qsv"         : Intel Quick Sync Video hardware acceleration
#   - "nvenc"       : NVIDIA NVENC hardware acceleration
#   - "vaapi"       : Video Acceleration API (Intel/AMD GPUs on Linux)
#   - "videotoolbox": Apple VideoToolbox hardware acceleration (macOS)
# Default: "cpu"
# engine = "cpu"

# vaapi_device: Hardware device path for VAAPI encoding
# Only used when engine = "vaapi"
# Default: "/dev/dri/renderD128"
# Examples:
#   - "/dev/dri/renderD128" (first GPU)
#   - "/dev/dri/renderD129" (second GPU)
# vaapi_device = "/dev/dri/renderD128"

# ============================================================================
# PROCESSING OPTIONS
# ============================================================================

# workers: Number of parallel transcoding workers
# Default: number of CPU cores
# workers = 4

# scan_interval: Time in minutes between directory scans
# Set to 0 to scan once and exit (no continuous monitoring)
# Default: 5
# scan_interval = 5

# interactive: Prompt for confirmation before starting
# Default: false
# interactive = false

# silent: Suppress progress output
# Default: false
# silent = false

# debug: Enable verbose debug output
# Default: false
# debug = false

# fast_mode: Use faster encoding settings (slightly lower quality)
# Increases CRF/ICQ/CQ values by 1 for faster encoding
# Default: false
# fast_mode = false

# ============================================================================
# OUTPUT OPTIONS
# ============================================================================

# force_mp4: Force MP4 container for all output files
# Default: false (uses MKV except for existing MP4 files)
# force_mp4 = false

# faststart_mp4: Enable faststart flag for MP4 files
# Moves moov atom to start of file for faster streaming
# Default: true
# faststart_mp4 = true

# ============================================================================
# ADVANCED OPTIONS
# ============================================================================

# keep: Keep intermediate files on error (for debugging)
# Default: false
# keep = false

# ============================================================================
# EXAMPLE CONFIGURATIONS
# ============================================================================

# Example 1: Fast CPU encoding with MP4 output
# engine = "cpu"
# fast_mode = true
# force_mp4 = true
# workers = 8

# Example 2: NVIDIA GPU acceleration
# engine = "nvenc"
# workers = 2
# force_mp4 = true

# Example 3: Intel hardware acceleration
# engine = "qsv"
# workers = 4

# Example 4: AMD GPU with VAAPI (Linux)
# engine = "vaapi"
# vaapi_device = "/dev/dri/renderD128"
# workers = 4

# Example 5: Apple Silicon or Intel Mac with VideoToolbox (macOS)
# engine = "videotoolbox"
# workers = 4

# Example 6: Multiple source directories
# source_dirs = ["/mnt/tv", "/mnt/movies", "/home/user/videos"]
# work_dir = "/mnt/transcoded"
# engine = "vaapi"
# workers = 2
`

	// Write the file
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// MergeConfigs merges a config file with CLI flags.
// CLI flags take precedence over config file values.
// Only explicitly set CLI flag values (non-default) override config file values.
func MergeConfigs(configFile, cliFlags *Config) *Config {
	// Start with config file values
	merged := *configFile

	// Override with CLI flags if they were explicitly set
	// For SourceDirs slice, override if CLI has any values
	if len(cliFlags.SourceDirs) > 0 {
		merged.SourceDirs = cliFlags.SourceDirs
	}
	if cliFlags.WorkDir != "" {
		merged.WorkDir = cliFlags.WorkDir
	}
	// For booleans, we only override if true (since false is the default)
	if cliFlags.Interactive {
		merged.Interactive = true
	}
	if cliFlags.Keep {
		merged.Keep = true
	}
	if cliFlags.Silent {
		merged.Silent = true
	}
	// For Workers, override if different from default (1)
	if cliFlags.Workers != 1 {
		merged.Workers = cliFlags.Workers
	}
	// For ScanInterval, override if different from default (5)
	if cliFlags.ScanInterval != 5 {
		merged.ScanInterval = cliFlags.ScanInterval
	}
	// For Engine, override if different from default ("cpu")
	if cliFlags.Engine != "" && cliFlags.Engine != "cpu" {
		merged.Engine = cliFlags.Engine
	}
	if cliFlags.VAAPIDevice != "" {
		merged.VAAPIDevice = cliFlags.VAAPIDevice
	}
	// For FFmpegPath, override if different from default ("ffmpeg")
	if cliFlags.FFmpegPath != "" && cliFlags.FFmpegPath != "ffmpeg" {
		merged.FFmpegPath = cliFlags.FFmpegPath
	}
	if cliFlags.FFprobePath != "" {
		merged.FFprobePath = cliFlags.FFprobePath
	}
	if cliFlags.Debug {
		merged.Debug = true
	}
	if cliFlags.ForceMP4 {
		merged.ForceMP4 = true
	}
	if cliFlags.FaststartMP4 {
		merged.FaststartMP4 = true
	}
	if cliFlags.FastMode {
		merged.FastMode = true
	}

	return &merged
}
