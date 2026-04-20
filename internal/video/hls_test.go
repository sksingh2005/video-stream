package video

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sksingh2005/video-stream/internal/config"
)

func TestSelectVariantsFiltersBySourceSize(t *testing.T) {
	variants := SelectVariants(1280, 720, config.LoadDefaultVariantsForTests())
	if len(variants) != 3 {
		t.Fatalf("expected 3 variants, got %d", len(variants))
	}
	if variants[0].Name != "720p" {
		t.Fatalf("expected 720p first variant, got %s", variants[0].Name)
	}
}

func TestWriteMasterPlaylist(t *testing.T) {
	dir := t.TempDir()
	variants := []RenderedVariant{
		{Profile: config.VariantProfile{Name: "720p", Width: 1280, Height: 720, VideoRate: "2800k", AudioRate: "128k"}, RelativePlaylist: "720p/index.m3u8"},
		{Profile: config.VariantProfile{Name: "480p", Width: 854, Height: 480, VideoRate: "1400k", AudioRate: "128k"}, RelativePlaylist: "480p/index.m3u8"},
	}

	if err := writeMasterPlaylist(dir, variants); err != nil {
		t.Fatalf("write master playlist: %v", err)
	}

	payload, err := os.ReadFile(filepath.Join(dir, "master.m3u8"))
	if err != nil {
		t.Fatalf("read playlist: %v", err)
	}

	if !strings.Contains(string(payload), "720p/index.m3u8") {
		t.Fatalf("master playlist missing 720p variant")
	}
}
