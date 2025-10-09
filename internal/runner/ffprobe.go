package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type ffProbeOutput struct {
	Streams []ffProbeStream `json:"streams"`
	Format  struct {
		Filename string `json:"filename"`
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
}

type ffProbeStream struct {
	CodecType    string `json:"codec_type"`
	CodecName    string `json:"codec_name"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	RFrameRate   string `json:"r_frame_rate"`
	AvgFrameRate string `json:"avg_frame_rate"`
	BitRate      string `json:"bit_rate"`
}

type ProbeInfo struct {
	VideoCodec string
	Width      int
	Height     int
	FPS        float64
	BitRate    int64
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

func probe(ctx context.Context, ffprobePath, file string) (*ProbeInfo, error) {
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
	fps := fpsFromFrac(v.RFrameRate)
	if fps == 0 {
		fps = fpsFromFrac(v.AvgFrameRate)
	}
	var bitrate int64
	if v.BitRate != "" {
		if br, err := strconv.ParseInt(v.BitRate, 10, 64); err == nil {
			bitrate = br
		}
	}
	if bitrate == 0 && out.Format.BitRate != "" {
		if br, err := strconv.ParseInt(out.Format.BitRate, 10, 64); err == nil {
			bitrate = br
		}
	}
	return &ProbeInfo{
		VideoCodec: v.CodecName,
		Width:      v.Width,
		Height:     v.Height,
		FPS:        fps,
		BitRate:    bitrate,
	}, nil
}
