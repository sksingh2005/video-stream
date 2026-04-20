package video

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sksingh2005/video-stream/internal/config"
	"github.com/sksingh2005/video-stream/internal/storage"
)

var ErrInvalidRequest = errors.New("invalid request")

type Service struct {
	cfg config.Config
	r2  r2Storage
}

type r2Storage interface {
	UploadDirVerified(ctx context.Context, localDir, remotePrefix string) ([]storage.UploadedObject, error)
}

type ProcessRequest struct {
	VideoID              string `json:"videoId"`
	SourcePath           string `json:"sourcePath"`
	ThumbnailTimeSeconds int    `json:"thumbnailTimeSeconds,omitempty"`
	CleanupSource        bool   `json:"cleanupSource,omitempty"`
}

type ProcessResponse struct {
	VideoID       string          `json:"videoId"`
	VideoPath     string          `json:"videoPath"`
	ThumbnailPath string          `json:"thumbnailPath,omitempty"`
	Duration      int             `json:"duration,omitempty"`
	Variants      []VariantResult `json:"variants,omitempty"`
}

type VariantResult struct {
	Name     string `json:"name"`
	Playlist string `json:"playlist"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

var (
	probeSource  = Probe
	transcodeHLS = TranscodeHLS
)

func NewService(cfg config.Config, r2 r2Storage) *Service {
	return &Service{cfg: cfg, r2: r2}
}

func (s *Service) ProcessAndUpload(ctx context.Context, req ProcessRequest) (ProcessResponse, error) {
	if strings.TrimSpace(req.VideoID) == "" || strings.TrimSpace(req.SourcePath) == "" {
		return ProcessResponse{}, fmt.Errorf("%w: videoId and sourcePath are required", ErrInvalidRequest)
	}

	if _, err := os.Stat(req.SourcePath); err != nil {
		return ProcessResponse{}, fmt.Errorf("%w: sourcePath is not readable: %v", ErrInvalidRequest, err)
	}

	videoID, err := sanitizeVideoID(req.VideoID)
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	workDir, err := os.MkdirTemp(s.cfg.Video.WorkingDir, "hls-*")
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("create temp working dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	sourceMeta, err := probeSource(ctx, s.cfg.Video.FFprobeBinary, req.SourcePath)
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("probe source video: %w", err)
	}

	selectedVariants := SelectVariants(sourceMeta.Width, sourceMeta.Height, s.cfg.Video.VariantBitrates)
	if len(selectedVariants) == 0 {
		return ProcessResponse{}, fmt.Errorf("no HLS variants selected for source dimensions %dx%d", sourceMeta.Width, sourceMeta.Height)
	}

	rendered, err := transcodeHLS(ctx, TranscodeRequest{
		FFmpegBinary:    s.cfg.Video.FFmpegBinary,
		SourcePath:      req.SourcePath,
		WorkDir:         workDir,
		SegmentLength:   s.cfg.Video.SegmentLength,
		ThumbnailAt:     s.thumbnailOffset(req.ThumbnailTimeSeconds),
		VariantProfiles: selectedVariants,
	})
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("transcode hls: %w", err)
	}

	remotePrefix := filepath.ToSlash(filepath.Join(s.cfg.Upload.Prefix, videoID))
	uploadedObjects, err := s.r2.UploadDirVerified(ctx, rendered.OutputDir, remotePrefix)
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("upload assets to r2: %w", err)
	}
	if len(uploadedObjects) == 0 {
		return ProcessResponse{}, fmt.Errorf("upload assets to r2: no files were uploaded")
	}

	resp := ProcessResponse{
		VideoID:   videoID,
		VideoPath: filepath.ToSlash(filepath.Join(remotePrefix, "master.m3u8")),
		Duration:  int(sourceMeta.DurationSeconds + 0.5),
	}

	if rendered.ThumbnailFile != "" {
		resp.ThumbnailPath = filepath.ToSlash(filepath.Join(remotePrefix, filepath.Base(rendered.ThumbnailFile)))
	}

	resp.Variants = make([]VariantResult, 0, len(rendered.Variants))
	for _, item := range rendered.Variants {
		resp.Variants = append(resp.Variants, VariantResult{
			Name:     item.Profile.Name,
			Playlist: filepath.ToSlash(filepath.Join(remotePrefix, item.RelativePlaylist)),
			Width:    item.Profile.Width,
			Height:   item.Profile.Height,
		})
	}

	if req.CleanupSource {
		if err := os.Remove(req.SourcePath); err != nil {
			return ProcessResponse{}, fmt.Errorf("cleanup source video after verified upload: %w", err)
		}
	}

	return resp, nil
}

func (s *Service) thumbnailOffset(explicitSeconds int) int {
	if explicitSeconds > 0 {
		return explicitSeconds
	}
	return int(s.cfg.Video.ThumbnailAt.Seconds())
}
