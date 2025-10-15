package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type ffProbeOutput struct {
	Streams []ffProbeStream `json:"streams"`
	Format  struct {
		Filename   string `json:"filename"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
		FormatName string `json:"format_name"`
	} `json:"format"`
}

type ffProbeStream struct {
	CodecType          string `json:"codec_type"`
	CodecName          string `json:"codec_name"`
	Width              int    `json:"width"`
	Height             int    `json:"height"`
	RFrameRate         string `json:"r_frame_rate"`
	AvgFrameRate       string `json:"avg_frame_rate"`
	BitRate            string `json:"bit_rate"`
	Profile            string `json:"profile"`
	PixFmt             string `json:"pix_fmt"`
	ColorSpace         string `json:"color_space"`
	ColorRange         string `json:"color_range"`
	ColorPrimaries     string `json:"color_primaries"`
	ChromaLocation     string `json:"chroma_location"`
	BitsPerRawSample   string `json:"bits_per_raw_sample"`
	CodedWidth         int    `json:"coded_width"`
	CodedHeight        int    `json:"coded_height"`
	DisplayAspectRatio string `json:"display_aspect_ratio"`
}

type ProbeInfo struct {
	VideoCodec string
	Width      int
	Height     int
	FPS        float64
	BitRate    int64
}

type ProbeInfoDetailed struct {
	// Container/Format
	Duration      float64
	FormatBitrate int64
	Container     string

	// Video Stream
	VideoCodec   string
	VideoProfile string
	Width        int
	Height       int
	CodedWidth   int
	CodedHeight  int
	FPS          float64
	AspectRatio  string
	VideoBitrate int64

	// Pixel/Color
	PixelFormat    string
	BitDepth       int
	ChromaLocation string
	ColorSpace     string
	ColorRange     string
	ColorPrimaries string
}

// fpsFromFrac parses "24000/1001" → 23.976, "25/1" → 25, "0/0" → 0
func fpsFromFrac(fr string) float64 {
	parts := strings.Split(fr, "/")
	if len(parts) != 2 {
		return 0
	}
	n, _ := strconv.ParseFloat(parts[0], 64)
	d, _ := strconv.ParseFloat(parts[1], 64)
	if n == 0 || d == 0 {
		return 0
	}
	return n / d
}

func probe(ctx context.Context, ffprobePath, file string) (*ProbeInfoDetailed, error) {
	args := []string{
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		file,
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffprobePath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe failed: %w (%s)", err, stderr.String())
	}
	var out ffProbeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("parse ffprobe: %w", err)
	}
	var v *ffProbeStream
	for i := range out.Streams {
		if out.Streams[i].CodecType == "video" {
			v = &out.Streams[i]
			break
		}
	}
	if v == nil {
		return nil, fmt.Errorf("no video stream")
	}

	// Parse FPS
	fps := fpsFromFrac(v.RFrameRate)
	if fps == 0 {
		fps = fpsFromFrac(v.AvgFrameRate)
	}

	// Parse video bitrate
	var videoBitrate int64
	if v.BitRate != "" {
		if br, err := strconv.ParseInt(v.BitRate, 10, 64); err == nil {
			videoBitrate = br
		}
	}

	// Parse format bitrate
	var formatBitrate int64
	if out.Format.BitRate != "" {
		if br, err := strconv.ParseInt(out.Format.BitRate, 10, 64); err == nil {
			formatBitrate = br
		}
	}

	// If video bitrate is not set, use format bitrate
	if videoBitrate == 0 {
		videoBitrate = formatBitrate
	}

	// Parse duration
	var duration float64
	if out.Format.Duration != "" {
		if d, err := strconv.ParseFloat(out.Format.Duration, 64); err == nil {
			duration = d
		}
	}

	// Extract container from filename extension or format name
	container := strings.ToLower(filepath.Ext(out.Format.Filename))
	if container != "" && strings.HasPrefix(container, ".") {
		container = container[1:] // Remove leading dot
	}
	if container == "" && out.Format.FormatName != "" {
		// Use first format name if extension not available
		container = strings.Split(out.Format.FormatName, ",")[0]
	}

	// Parse bit depth from BitsPerRawSample
	var bitDepth int
	if v.BitsPerRawSample != "" {
		if bd, err := strconv.Atoi(v.BitsPerRawSample); err == nil {
			bitDepth = bd
		}
	}

	// Use coded dimensions if available, otherwise use regular dimensions
	codedWidth := v.CodedWidth
	if codedWidth == 0 {
		codedWidth = v.Width
	}
	codedHeight := v.CodedHeight
	if codedHeight == 0 {
		codedHeight = v.Height
	}

	return &ProbeInfoDetailed{
		// Container/Format
		Duration:      duration,
		FormatBitrate: formatBitrate,
		Container:     container,

		// Video Stream
		VideoCodec:   v.CodecName,
		VideoProfile: v.Profile,
		Width:        v.Width,
		Height:       v.Height,
		CodedWidth:   codedWidth,
		CodedHeight:  codedHeight,
		FPS:          fps,
		AspectRatio:  v.DisplayAspectRatio,
		VideoBitrate: videoBitrate,

		// Pixel/Color
		PixelFormat:    v.PixFmt,
		BitDepth:       bitDepth,
		ChromaLocation: v.ChromaLocation,
		ColorSpace:     v.ColorSpace,
		ColorRange:     v.ColorRange,
		ColorPrimaries: v.ColorPrimaries,
	}, nil
}
