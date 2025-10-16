package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Real ffprobe output from: Gen V - S01E03 - #ThinkBrink Bluray-1080p.mkv.
const realWorldFFProbeOutput = `{
    "streams": [
        {
            "index": 0,
            "codec_name": "hevc",
            "codec_long_name": "H.265 / HEVC (High Efficiency Video Coding)",
            "profile": "Main",
            "codec_type": "video",
            "codec_tag_string": "[0][0][0][0]",
            "codec_tag": "0x0000",
            "width": 1920,
            "height": 1080,
            "coded_width": 1920,
            "coded_height": 1088,
            "has_b_frames": 1,
            "sample_aspect_ratio": "1:1",
            "display_aspect_ratio": "16:9",
            "pix_fmt": "yuv420p",
            "level": 120,
            "color_range": "tv",
            "color_space": "bt709",
            "color_transfer": "bt709",
            "color_primaries": "bt709",
            "chroma_location": "left",
            "field_order": "progressive",
            "refs": 1,
            "r_frame_rate": "24000/1001",
            "avg_frame_rate": "24000/1001",
            "time_base": "1/1000",
            "start_pts": 0,
            "start_time": "0.000000",
            "extradata_size": 127,
            "disposition": {
                "default": 1,
                "dub": 0,
                "original": 0,
                "comment": 0,
                "lyrics": 0,
                "karaoke": 0,
                "forced": 0,
                "hearing_impaired": 0,
                "visual_impaired": 0,
                "clean_effects": 0,
                "attached_pic": 0,
                "timed_thumbnails": 0,
                "non_diegetic": 0,
                "captions": 0,
                "descriptions": 0,
                "metadata": 0,
                "dependent": 0,
                "still_image": 0,
                "multilayer": 0
            },
            "tags": {
                "ENCODER": "Lavc62.11.100 hevc_vaapi",
                "BPS": "11727826",
                "BPS-eng": "11727826",
                "DURATION-eng": "00:48:23.943000000",
                "NUMBER_OF_FRAMES": "69625",
                "NUMBER_OF_FRAMES-eng": "69625",
                "NUMBER_OF_BYTES": "4257117498",
                "NUMBER_OF_BYTES-eng": "4257117498",
                "_STATISTICS_WRITING_APP": "DVDFab 12.1.1.5",
                "_STATISTICS_WRITING_APP-eng": "DVDFab 12.1.1.5",
                "_STATISTICS_WRITING_DATE_UTC": "2025-03-02 15:16:29",
                "_STATISTICS_WRITING_DATE_UTC-eng": "2025-03-02 15:16:29",
                "_STATISTICS_TAGS": "BPS DURATION NUMBER_OF_FRAMES NUMBER_OF_BYTES",
                "_STATISTICS_TAGS-eng": "BPS DURATION NUMBER_OF_FRAMES NUMBER_OF_BYTES",
                "DURATION": "00:00:14.056000000"
            }
        },
        {
            "index": 1,
            "codec_name": "dts",
            "codec_long_name": "DCA (DTS Coherent Acoustics)",
            "profile": "DTS-HD MA",
            "codec_type": "audio",
            "codec_tag_string": "[0][0][0][0]",
            "codec_tag": "0x0000",
            "sample_fmt": "s32p",
            "sample_rate": "48000",
            "channels": 6,
            "channel_layout": "5.1(side)",
            "bits_per_sample": 0,
            "initial_padding": 0,
            "r_frame_rate": "0/0",
            "avg_frame_rate": "0/0",
            "time_base": "1/1000",
            "start_pts": 0,
            "start_time": "0.000000",
            "bits_per_raw_sample": "24",
            "disposition": {
                "default": 1,
                "dub": 0,
                "original": 0,
                "comment": 0,
                "lyrics": 0,
                "karaoke": 0,
                "forced": 0,
                "hearing_impaired": 0,
                "visual_impaired": 0,
                "clean_effects": 0,
                "attached_pic": 0,
                "timed_thumbnails": 0,
                "non_diegetic": 0,
                "captions": 0,
                "descriptions": 0,
                "metadata": 0,
                "dependent": 0,
                "still_image": 0,
                "multilayer": 0
            },
            "tags": {
                "language": "eng",
                "BPS": "3628634",
                "BPS-eng": "3628634",
                "DURATION-eng": "00:48:23.862000000",
                "NUMBER_OF_FRAMES": "272237",
                "NUMBER_OF_FRAMES-eng": "272237",
                "NUMBER_OF_BYTES": "1317131592",
                "NUMBER_OF_BYTES-eng": "1317131592",
                "_STATISTICS_WRITING_APP": "DVDFab 12.1.1.5",
                "_STATISTICS_WRITING_APP-eng": "DVDFab 12.1.1.5",
                "_STATISTICS_WRITING_DATE_UTC": "2025-03-02 15:16:29",
                "_STATISTICS_WRITING_DATE_UTC-eng": "2025-03-02 15:16:29",
                "_STATISTICS_TAGS": "BPS DURATION NUMBER_OF_FRAMES NUMBER_OF_BYTES",
                "_STATISTICS_TAGS-eng": "BPS DURATION NUMBER_OF_FRAMES NUMBER_OF_BYTES",
                "DURATION": "00:00:14.057000000"
            }
        }
    ],
    "format": {
        "filename": "/mnt/cold/tv/Gen V/Season 1/Gen V - S01E03 - #ThinkBrink Bluray-1080p.mkv",
        "nb_streams": 2,
        "nb_programs": 0,
        "nb_stream_groups": 0,
        "format_name": "matroska,webm",
        "format_long_name": "Matroska / WebM",
        "start_time": "0.000000",
        "duration": "14.057000",
        "size": "10636031",
        "bit_rate": "6053087",
        "probe_score": 100,
        "tags": {
            "title": "Gen.V.S01E03.ThinkBrink.1080p.BluRay.Dts-HDMa5.1.AVC-PiR8",
            "ENCODER": "Lavf62.3.100"
        }
    }
}`

const noVideoStreamFFProbeOutput = `{
    "streams": [
        {
            "index": 0,
            "codec_name": "aac",
            "codec_type": "audio",
            "sample_rate": "48000",
            "channels": 2
        }
    ],
    "format": {
        "filename": "audio_only.mp4",
        "duration": "120.5",
        "bit_rate": "128000"
    }
}`

func TestFpsFromFrac(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected float64
	}{
		{
			name:     "23.976 fps (NTSC film)",
			input:    "24000/1001",
			expected: 23.976023976023978,
		},
		{
			name:     "25 fps (PAL)",
			input:    "25/1",
			expected: 25.0,
		},
		{
			name:     "30 fps",
			input:    "30/1",
			expected: 30.0,
		},
		{
			name:     "29.97 fps (NTSC)",
			input:    "30000/1001",
			expected: 29.97002997002997,
		},
		{
			name:     "zero/zero returns 0",
			input:    "0/0",
			expected: 0.0,
		},
		{
			name:     "zero numerator returns 0",
			input:    "0/1",
			expected: 0.0,
		},
		{
			name:     "zero denominator returns 0",
			input:    "25/0",
			expected: 0.0,
		},
		{
			name:     "malformed input (no slash)",
			input:    "25",
			expected: 0.0,
		},
		{
			name:     "malformed input (too many parts)",
			input:    "25/1/1",
			expected: 0.0,
		},
		{
			name:     "empty string",
			input:    "",
			expected: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := fpsFromFrac(tt.input)
			if result != tt.expected {
				t.Errorf("fpsFromFrac(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestProbeWithRealWorldData(t *testing.T) {
	// Parse the JSON to simulate what probe() does
	var out ffProbeOutput
	if err := json.Unmarshal([]byte(realWorldFFProbeOutput), &out); err != nil {
		t.Fatalf("failed to parse test JSON: %v", err)
	}

	// Find video stream (same logic as probe())
	var v *ffProbeStream
	for i := range out.Streams {
		if out.Streams[i].CodecType == "video" {
			v = &out.Streams[i]
			break
		}
	}

	if v == nil {
		t.Fatal("expected to find video stream in test data")
	}

	// Test FPS parsing
	fps := fpsFromFrac(v.RFrameRate)
	expectedFPS := 23.976023976023978
	if fps != expectedFPS {
		t.Errorf("FPS = %v, want %v", fps, expectedFPS)
	}

	// Test basic video properties
	if v.CodecName != "hevc" {
		t.Errorf("VideoCodec = %q, want %q", v.CodecName, "hevc")
	}
	if v.Profile != "Main" {
		t.Errorf("Profile = %q, want %q", v.Profile, "Main")
	}
	if v.Width != 1920 {
		t.Errorf("Width = %d, want %d", v.Width, 1920)
	}
	if v.Height != 1080 {
		t.Errorf("Height = %d, want %d", v.Height, 1080)
	}
	if v.CodedWidth != 1920 {
		t.Errorf("CodedWidth = %d, want %d", v.CodedWidth, 1920)
	}
	if v.CodedHeight != 1088 {
		t.Errorf("CodedHeight = %d, want %d", v.CodedHeight, 1088)
	}
	if v.DisplayAspectRatio != "16:9" {
		t.Errorf("AspectRatio = %q, want %q", v.DisplayAspectRatio, "16:9")
	}

	// Test pixel/color properties
	if v.PixFmt != "yuv420p" {
		t.Errorf("PixelFormat = %q, want %q", v.PixFmt, "yuv420p")
	}
	if v.ColorSpace != "bt709" {
		t.Errorf("ColorSpace = %q, want %q", v.ColorSpace, "bt709")
	}
	if v.ColorRange != "tv" {
		t.Errorf("ColorRange = %q, want %q", v.ColorRange, "tv")
	}
	if v.ColorPrimaries != "bt709" {
		t.Errorf("ColorPrimaries = %q, want %q", v.ColorPrimaries, "bt709")
	}
	if v.ChromaLocation != "left" {
		t.Errorf("ChromaLocation = %q, want %q", v.ChromaLocation, "left")
	}

	// Test format properties
	if out.Format.Duration != "14.057000" {
		t.Errorf("Duration string = %q, want %q", out.Format.Duration, "14.057000")
	}

	if out.Format.BitRate != "6053087" {
		t.Errorf("BitRate string = %q, want %q", out.Format.BitRate, "6053087")
	}

	// Test container extraction
	container := filepath.Ext(out.Format.Filename)
	if len(container) > 0 && container[0] == '.' {
		container = container[1:]
	}
	if container != "mkv" {
		t.Errorf("Container = %q, want %q", container, "mkv")
	}

	t.Logf("Successfully validated all fields from real-world ffprobe output")
	t.Logf("Video: %s (%s), %dx%d @ %.3f fps", v.CodecName, v.Profile, v.Width, v.Height, fps)
	t.Logf("Duration: %s seconds, Bitrate: %s bps", out.Format.Duration, out.Format.BitRate)
}

func TestProbeNoVideoStream(t *testing.T) {
	// Parse JSON with no video stream
	var out ffProbeOutput
	if err := json.Unmarshal([]byte(noVideoStreamFFProbeOutput), &out); err != nil {
		t.Fatalf("failed to parse test JSON: %v", err)
	}

	// Try to find video stream
	var v *ffProbeStream
	for i := range out.Streams {
		if out.Streams[i].CodecType == "video" {
			v = &out.Streams[i]
			break
		}
	}

	// Should not find a video stream
	if v != nil {
		t.Error("expected no video stream, but found one")
	}
}

// TestProbeIntegration tests the actual probe() function if ffprobe is available.
func TestProbeIntegration(t *testing.T) {
	// Check if ffprobe is available
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found in PATH, skipping integration test")
	}

	// Check if test video file exists
	testFile := "/mnt/cold/tv/Gen V/Season 1/Gen V - S01E03 - #ThinkBrink Bluray-1080p.mkv"
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Skip("test video file not found, skipping integration test")
	}

	// Run actual probe
	ctx := context.Background()
	info, err := probe(ctx, ffprobePath, testFile)
	if err != nil {
		t.Fatalf("probe() failed: %v", err)
	}

	// Verify expected values
	if info.VideoCodec != "hevc" {
		t.Errorf("VideoCodec = %q, want %q", info.VideoCodec, "hevc")
	}
	if info.Width != 1920 {
		t.Errorf("Width = %d, want %d", info.Width, 1920)
	}
	if info.Height != 1080 {
		t.Errorf("Height = %d, want %d", info.Height, 1080)
	}

	// FPS should be approximately 23.976
	expectedFPS := 23.976023976023978
	if info.FPS != expectedFPS {
		t.Errorf("FPS = %v, want %v", info.FPS, expectedFPS)
	}

	if info.PixelFormat != "yuv420p" {
		t.Errorf("PixelFormat = %q, want %q", info.PixelFormat, "yuv420p")
	}

	if info.ColorSpace != "bt709" {
		t.Errorf("ColorSpace = %q, want %q", info.ColorSpace, "bt709")
	}

	if info.Container != "mkv" {
		t.Errorf("Container = %q, want %q", info.Container, "mkv")
	}

	t.Logf("Integration test passed!")
	t.Logf("Probed: %s (%s), %dx%d @ %.3f fps", info.VideoCodec, info.VideoProfile, info.Width, info.Height, info.FPS)
	t.Logf(
		"Color: %s/%s, Bitrate: %d bps, Duration: %.2f sec",
		info.ColorSpace,
		info.PixelFormat,
		info.VideoBitrate,
		info.Duration,
	)
}
