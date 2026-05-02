package video

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sksingh2005/video-stream/internal/config"
	"github.com/sksingh2005/video-stream/internal/storage"
)

type fakeR2Storage struct {
	uploaded []storage.UploadedObject
	err      error
	deleted  []string
	copied   []string
	cleanup  []string
}

func (f *fakeR2Storage) UploadDirVerified(_ context.Context, _ string, _ string) ([]storage.UploadedObject, error) {
	return f.uploaded, f.err
}

func (f *fakeR2Storage) CopyPrefixVerified(_ context.Context, _ string, destinationPrefix string) ([]storage.UploadedObject, error) {
	f.copied = append(f.copied, destinationPrefix)
	return f.uploaded, f.err
}

func (f *fakeR2Storage) DeletePrefix(_ context.Context, prefix string) error {
	f.deleted = append(f.deleted, prefix)
	return nil
}

func (f *fakeR2Storage) DeletePrefixContentsExcept(_ context.Context, prefix, keepPrefix string) error {
	f.cleanup = append(f.cleanup, prefix+"|"+keepPrefix)
	return nil
}

func TestProcessAndUploadDeletesSourceOnlyAfterVerifiedUpload(t *testing.T) {
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "lesson.mp4")
	if err := os.WriteFile(sourcePath, []byte("video"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outputDir, "master.m3u8"), []byte("#EXTM3U"), 0o644); err != nil {
		t.Fatalf("write master manifest: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(outputDir, "720p"), 0o755); err != nil {
		t.Fatalf("create variant dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "720p", "index.m3u8"), []byte("#EXTM3U"), 0o644); err != nil {
		t.Fatalf("write variant manifest: %v", err)
	}

	originalProbe := probeSource
	originalTranscode := transcodeHLS
	t.Cleanup(func() {
		probeSource = originalProbe
		transcodeHLS = originalTranscode
	})
	probeSource = func(context.Context, string, string) (ProbeResult, error) {
		return ProbeResult{Width: 1280, Height: 720, DurationSeconds: 10.5}, nil
	}
	transcodeHLS = func(context.Context, TranscodeRequest) (TranscodeResult, error) {
		return TranscodeResult{
			OutputDir: outputDir,
			Variants: []RenderedVariant{
				{
					Profile:          config.VariantProfile{Name: "720p", Width: 1280, Height: 720, VideoRate: "2800k", MaxRate: "2996k", Buffer: "4200k", AudioRate: "128k"},
					RelativePlaylist: "720p/index.m3u8",
				},
			},
		}, nil
	}

	service := NewService(config.Config{
		Upload: config.UploadConfig{Prefix: "videos"},
		Video: config.VideoConfig{
			FFmpegBinary:    "ffmpeg",
			FFprobeBinary:   "ffprobe",
			WorkingDir:      t.TempDir(),
			SegmentLength:   6,
			VariantBitrates: []config.VariantProfile{{Name: "720p", Width: 1280, Height: 720, VideoRate: "2800k", MaxRate: "2996k", Buffer: "4200k", AudioRate: "128k"}},
		},
	}, &fakeR2Storage{
		uploaded: []storage.UploadedObject{{ObjectKey: "videos/lesson-123/master.m3u8", Size: 8}},
	})

	resp, err := service.ProcessAndUpload(context.Background(), ProcessRequest{
		VideoID:       "lesson-123",
		SourcePath:    sourcePath,
		CleanupSource: true,
	})
	if err != nil {
		t.Fatalf("process and upload: %v", err)
	}

	if _, err := os.Stat(sourcePath); !os.IsNotExist(err) {
		t.Fatalf("expected source file to be removed, got err=%v", err)
	}

	if len(service.r2.(*fakeR2Storage).copied) != 1 {
		t.Fatalf("expected published prefix to be copied once")
	}
	if resp.VideoPath == "videos/lesson-123/master.m3u8" {
		t.Fatalf("expected versioned publish path, got %s", resp.VideoPath)
	}
	if len(service.r2.(*fakeR2Storage).cleanup) != 1 {
		t.Fatalf("expected old published versions cleanup to run once")
	}
}

func TestProcessAndUploadKeepsSourceWhenVerifiedUploadFails(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "lesson.mp4")
	if err := os.WriteFile(sourcePath, []byte("video"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outputDir, "master.m3u8"), []byte("#EXTM3U"), 0o644); err != nil {
		t.Fatalf("write master manifest: %v", err)
	}

	originalProbe := probeSource
	originalTranscode := transcodeHLS
	t.Cleanup(func() {
		probeSource = originalProbe
		transcodeHLS = originalTranscode
	})
	probeSource = func(context.Context, string, string) (ProbeResult, error) {
		return ProbeResult{Width: 1280, Height: 720, DurationSeconds: 10.5}, nil
	}
	transcodeHLS = func(context.Context, TranscodeRequest) (TranscodeResult, error) {
		return TranscodeResult{OutputDir: outputDir}, nil
	}

	service := NewService(config.Config{
		Upload: config.UploadConfig{Prefix: "videos"},
		Video: config.VideoConfig{
			FFmpegBinary:    "ffmpeg",
			FFprobeBinary:   "ffprobe",
			WorkingDir:      t.TempDir(),
			SegmentLength:   6,
			VariantBitrates: []config.VariantProfile{{Name: "720p", Width: 1280, Height: 720, VideoRate: "2800k", MaxRate: "2996k", Buffer: "4200k", AudioRate: "128k"}},
		},
	}, &fakeR2Storage{
		err: os.ErrPermission,
	})

	_, err := service.ProcessAndUpload(context.Background(), ProcessRequest{
		VideoID:       "lesson-123",
		SourcePath:    sourcePath,
		CleanupSource: true,
	})
	if err == nil {
		t.Fatalf("expected processing to fail")
	}

	if _, statErr := os.Stat(sourcePath); statErr != nil {
		t.Fatalf("expected source file to remain, got err=%v", statErr)
	}
}

func TestCleanupInterruptedUploadsDeletesRecordedPrefixes(t *testing.T) {
	r2 := &fakeR2Storage{}
	service := NewService(config.Config{
		Video: config.VideoConfig{
			WorkingDir: t.TempDir(),
		},
	}, r2)

	recoveryDir := service.recoveryDir()
	if err := os.MkdirAll(recoveryDir, 0o755); err != nil {
		t.Fatalf("create recovery dir: %v", err)
	}

	manifestPath := filepath.Join(recoveryDir, "content-123.manifest")
	if err := os.WriteFile(manifestPath, []byte("videos/content-123"), 0o644); err != nil {
		t.Fatalf("write recovery manifest: %v", err)
	}

	if err := service.CleanupInterruptedUploads(context.Background()); err != nil {
		t.Fatalf("cleanup interrupted uploads: %v", err)
	}

	if len(r2.deleted) != 1 || r2.deleted[0] != "videos/content-123" {
		t.Fatalf("unexpected deleted prefixes: %#v", r2.deleted)
	}

	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("expected manifest to be removed, got err=%v", err)
	}
}
