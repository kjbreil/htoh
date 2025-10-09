package runner

import "testing"

const sampleEncodersOutput = `Encoders:
 V....D hevc_amf             AMD AMF HEVC encoder (codec hevc)
 V....D hevc_nvenc           NVIDIA NVENC hevc encoder (codec hevc)
 V..... hevc_qsv             HEVC (Intel Quick Sync Video acceleration) (codec hevc)
`

func TestEngineOptionsFromEncodersDetectsNVENC(t *testing.T) {
	opts := engineOptionsFromEncoders(sampleEncodersOutput)
	found := make(map[string]bool)
	for _, opt := range opts {
		found[opt.Name] = true
	}
	if !found["nvenc"] {
		t.Fatalf("expected nvenc in options, got %#v", opts)
	}
	if !found["qsv"] {
		t.Fatalf("expected qsv in options, got %#v", opts)
	}
}
