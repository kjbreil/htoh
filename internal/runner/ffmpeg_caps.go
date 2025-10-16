package runner

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

const (
	defaultFFmpegBinary = "ffmpeg"
)

type EngineInfo struct {
	Name        string
	Description string
}

// PrintHardwareCaps runs ffmpeg discovery commands and prints potential hardware accelerators,
// hardware encoders, and the engines that opti can use with the supplied ffmpeg binary.
func PrintHardwareCaps(ffmpegPath string) error {
	if ffmpegPath == "" {
		ffmpegPath = defaultFFmpegBinary
	}
	accels, err := runFFmpegCapture(ffmpegPath, "-hide_banner", "-hwaccels")
	if err != nil {
		return fmt.Errorf("probe hardware accelerators: %w", err)
	}

	encoders, err := runFFmpegCapture(ffmpegPath, "-hide_banner", "-encoders")
	if err != nil {
		return fmt.Errorf("probe encoders: %w", err)
	}

	engineOpts := engineOptionsFromEncoders(encoders)
	fmt.Println("Engines available for -engine with this ffmpeg build:")
	for _, opt := range engineOpts {
		fmt.Printf("  %-6s %s\n", opt.Name, opt.Description)
	}
	fmt.Println()

	fmt.Println("Hardware accelerators reported by ffmpeg:")
	fmt.Println(strings.TrimSpace(accels))
	fmt.Println()

	fmt.Println("HEVC hardware encoders detected (from ffmpeg -encoders):")
	hwEnc := filterHEVCHardwareEncoders(encoders)
	for _, line := range hwEnc {
		fmt.Println("  " + line)
	}
	if len(hwEnc) == 0 {
		fmt.Println("  (none detected)")
	}
	return nil
}

// EngineOptions returns the set of engines opti knows how to drive, filtered by ffmpeg support.
func EngineOptions(ffmpegPath string) ([]EngineInfo, error) {
	if ffmpegPath == "" {
		ffmpegPath = defaultFFmpegBinary
	}
	encoders, err := runFFmpegCapture(ffmpegPath, "-hide_banner", "-encoders")
	if err != nil {
		return nil, fmt.Errorf("probe encoders: %w", err)
	}
	return engineOptionsFromEncoders(encoders), nil
}

// ValidateEngine ensures the requested engine is usable with this ffmpeg build.
func ValidateEngine(name, ffmpegPath string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == "cpu" {
		return nil
	}
	if name == "hevc_nvenc" {
		name = "nvenc"
	}
	if name == "hevc_vaapi" {
		name = "vaapi"
	}
	if name == "hevc_videotoolbox" {
		name = "videotoolbox"
	}
	opts, err := EngineOptions(ffmpegPath)
	if err != nil {
		return err
	}
	for _, opt := range opts {
		if opt.Name == name {
			return nil
		}
	}
	return fmt.Errorf(
		"engine %q is not available with ffmpeg %q; run opti -list-hw to inspect support",
		name,
		ffmpegPath,
	)
}

func engineOptionsFromEncoders(encoders string) []EngineInfo {
	out := []EngineInfo{
		{Name: "cpu", Description: "Software (libx265)"},
	}
	var (
		hasQSV         bool
		hasNVENC       bool
		hasVAAPI       bool
		hasVideoToolbox bool
	)
	for _, line := range filterHEVCHardwareEncoders(encoders) {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "hevc_qsv") {
			hasQSV = true
		}
		if strings.Contains(lower, "hevc_nvenc") {
			hasNVENC = true
		}
		if strings.Contains(lower, "hevc_vaapi") {
			hasVAAPI = true
		}
		if strings.Contains(lower, "hevc_videotoolbox") {
			hasVideoToolbox = true
		}
	}
	if hasQSV {
		out = append(out, EngineInfo{Name: "qsv", Description: "Intel Quick Sync (hevc_qsv)"})
	}
	if hasNVENC {
		out = append(out, EngineInfo{Name: "nvenc", Description: "NVIDIA NVENC (hevc_nvenc)"})
	}
	if hasVAAPI {
		out = append(out, EngineInfo{Name: "vaapi", Description: "VA-API (hevc_vaapi)"})
	}
	if hasVideoToolbox {
		out = append(out, EngineInfo{Name: "videotoolbox", Description: "Apple VideoToolbox (hevc_videotoolbox)"})
	}
	return out
}

func runFFmpegCapture(ffmpeg string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	// #nosec G204 - ffmpeg path comes from validated config or defaults to "ffmpeg"
	cmd := exec.CommandContext(context.Background(), ffmpeg, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return stdout.String(), nil
}

func filterHEVCHardwareEncoders(output string) []string {
	var out []string
	hwTokens := []string{
		"_nvenc",
		"_amf",
		"_qsv",
		"_vaapi",
		"_videotoolbox",
		"_v4l2m2m",
		"_mmal",
		"_omx",
		"_cuda",
		"_vdenc",
		"_dxva2",
	}
	sc := bufio.NewScanner(strings.NewReader(output))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "Encoders") {
			continue
		}
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "hevc") && !strings.Contains(lower, "h265") {
			continue
		}
		for _, token := range hwTokens {
			if strings.Contains(lower, token) {
				out = append(out, line)
				break
			}
		}
	}
	return out
}
