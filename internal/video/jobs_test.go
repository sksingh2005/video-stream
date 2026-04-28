package video

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sksingh2005/video-stream/internal/config"
	"github.com/sksingh2005/video-stream/internal/storage"
)

func TestJobManagerProcessesQueuedJob(t *testing.T) {
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
		uploaded: []storage.UploadedObject{{ObjectKey: "videos/lesson-123/master.m3u8", Size: 8}},
	})

	manager := NewJobManager(config.JobConfig{QueueSize: 2, RetentionMinutes: 60}, service)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx, 1)

	job, err := manager.Enqueue(ProcessRequest{
		VideoID:       "lesson-123",
		SourcePath:    sourcePath,
		CleanupSource: true,
	}, true)
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := manager.Get(job.ID)
		if !ok {
			t.Fatalf("expected job %s to exist", job.ID)
		}
		if current.Status == JobStatusSucceeded {
			if current.Result == nil || current.Result.VideoPath == "" {
				t.Fatalf("expected result payload, got %#v", current.Result)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("job %s did not finish in time", job.ID)
}

func TestJobManagerReturnsQueueFull(t *testing.T) {
	manager := NewJobManager(config.JobConfig{QueueSize: 1, RetentionMinutes: 60}, nil)

	_, err := manager.Enqueue(ProcessRequest{VideoID: "one", SourcePath: "C:\\temp\\one.mp4"}, false)
	if err != nil {
		t.Fatalf("first enqueue failed: %v", err)
	}

	_, err = manager.Enqueue(ProcessRequest{VideoID: "two", SourcePath: "C:\\temp\\two.mp4"}, false)
	if err == nil {
		t.Fatalf("expected second enqueue to fail")
	}
	if !errors.Is(err, ErrJobQueueFull) {
		t.Fatalf("expected ErrJobQueueFull, got %v", err)
	}
}

func TestJobManagerRejectsDuplicateActiveVideoID(t *testing.T) {
	manager := NewJobManager(config.JobConfig{QueueSize: 2, RetentionMinutes: 60}, nil)

	_, err := manager.Enqueue(ProcessRequest{VideoID: "lesson-123", SourcePath: "C:\\temp\\one.mp4"}, false)
	if err != nil {
		t.Fatalf("first enqueue failed: %v", err)
	}

	_, err = manager.Enqueue(ProcessRequest{VideoID: "lesson-123", SourcePath: "C:\\temp\\two.mp4"}, false)
	if err == nil {
		t.Fatalf("expected duplicate enqueue to fail")
	}
	if !errors.Is(err, ErrVideoAlreadyProcessing) {
		t.Fatalf("expected ErrVideoAlreadyProcessing, got %v", err)
	}
}

func TestJobManagerAllowsRetryAfterFailure(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "lesson.mp4")
	if err := os.WriteFile(sourcePath, []byte("video"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
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
		return TranscodeResult{}, errors.New("ffmpeg failed")
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
	}, &fakeR2Storage{})

	manager := NewJobManager(config.JobConfig{QueueSize: 2, RetentionMinutes: 60}, service)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx, 1)

	job, err := manager.Enqueue(ProcessRequest{
		VideoID:       "lesson-123",
		SourcePath:    sourcePath,
		CleanupSource: false,
	}, false)
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := manager.Get(job.ID)
		if !ok {
			t.Fatalf("expected job %s to exist", job.ID)
		}
		if current.Status == JobStatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, err = manager.Enqueue(ProcessRequest{VideoID: "lesson-123", SourcePath: "C:\\temp\\retry.mp4"}, false)
	if err != nil {
		t.Fatalf("expected retry enqueue to succeed, got %v", err)
	}
}
