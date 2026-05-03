package video

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	CopyPrefixVerified(ctx context.Context, sourcePrefix, destinationPrefix string) ([]storage.UploadedObject, error)
	DeletePrefix(ctx context.Context, prefix string) error
	DeletePrefixContentsExcept(ctx context.Context, prefix, keepPrefix string) error
	DeleteObject(ctx context.Context, objectKey string) error
	DownloadFile(ctx context.Context, objectKey, destinationPath string) error
}

type ProcessRequest struct {
	VideoID              string                `json:"videoId"`
	SourcePath           string                `json:"sourcePath"`
	SourceObjectKey      string                `json:"sourceObjectKey,omitempty"`
	ThumbnailTimeSeconds int                   `json:"thumbnailTimeSeconds,omitempty"`
	CleanupSource        bool                  `json:"cleanupSource,omitempty"`
	CleanupSourceObject  bool                  `json:"cleanupSourceObject,omitempty"`
	ProgressCallback     func(ProcessProgress) `json:"-"`
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

type ProcessProgress struct {
	Phase   string `json:"phase"`
	Percent int    `json:"percent"`
	Message string `json:"message,omitempty"`
}

type uploadRecoveryManifest struct {
	VideoID       string `json:"videoId"`
	StagingPrefix string `json:"stagingPrefix"`
	PublishPrefix string `json:"publishPrefix"`
}

var (
	probeSource  = Probe
	transcodeHLS = TranscodeHLS
)

func NewService(cfg config.Config, r2 r2Storage) *Service {
	return &Service{cfg: cfg, r2: r2}
}

func (s *Service) CreateSourceUploadFile(extension string) (*os.File, error) {
	if strings.TrimSpace(extension) == "" {
		extension = ".mp4"
	}

	if err := os.MkdirAll(s.cfg.Upload.SourceDir, 0o755); err != nil {
		return nil, fmt.Errorf("ensure source upload dir: %w", err)
	}

	file, err := os.CreateTemp(s.cfg.Upload.SourceDir, "video-upload-*"+extension)
	if err != nil {
		return nil, fmt.Errorf("create source upload file: %w", err)
	}
	return file, nil
}

func (s *Service) CleanupInterruptedUploads(ctx context.Context) error {
	recoveryDir := s.recoveryDir()
	entries, err := os.ReadDir(recoveryDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read upload recovery dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		manifestPath := filepath.Join(recoveryDir, entry.Name())
		payload, err := os.ReadFile(manifestPath)
		if err != nil {
			return fmt.Errorf("read upload recovery manifest %s: %w", manifestPath, err)
		}

		manifest, err := parseUploadRecoveryManifest(payload)
		if err != nil {
			return fmt.Errorf("parse upload recovery manifest %s: %w", manifestPath, err)
		}
		if manifest.StagingPrefix == "" && manifest.PublishPrefix == "" {
			if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove empty upload recovery manifest %s: %w", manifestPath, err)
			}
			continue
		}

		if manifest.StagingPrefix != "" {
			log.Printf("cleaning interrupted staging prefix=%s manifest=%s", manifest.StagingPrefix, entry.Name())
			if err := s.r2.DeletePrefix(ctx, manifest.StagingPrefix); err != nil {
				return fmt.Errorf("delete interrupted staging prefix %s: %w", manifest.StagingPrefix, err)
			}
		}
		if manifest.PublishPrefix != "" {
			log.Printf("cleaning interrupted publish prefix=%s manifest=%s", manifest.PublishPrefix, entry.Name())
			if err := s.r2.DeletePrefix(ctx, manifest.PublishPrefix); err != nil {
				return fmt.Errorf("delete interrupted publish prefix %s: %w", manifest.PublishPrefix, err)
			}
		}
		if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove upload recovery manifest %s: %w", manifestPath, err)
		}
	}

	return nil
}

func (s *Service) ProcessAndUpload(ctx context.Context, req ProcessRequest) (ProcessResponse, error) {
	if strings.TrimSpace(req.VideoID) == "" {
		return ProcessResponse{}, fmt.Errorf("%w: videoId is required", ErrInvalidRequest)
	}

	sourcePath, cleanupLocalSource, err := s.resolveSourcePath(ctx, req)
	if err != nil {
		return ProcessResponse{}, err
	}
	defer func() {
		if cleanupLocalSource {
			_ = os.Remove(sourcePath)
		}
	}()

	videoID, err := sanitizeVideoID(req.VideoID)
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	workDir, err := os.MkdirTemp(s.cfg.Video.WorkingDir, "hls-*")
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("create temp working dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	s.reportProgress(req, ProcessProgress{
		Phase:   "probing",
		Percent: 10,
		Message: "Inspecting source video",
	})
	sourceMeta, err := probeSource(ctx, s.cfg.Video.FFprobeBinary, sourcePath)
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("probe source video: %w", err)
	}

	selectedVariants := SelectVariants(sourceMeta.Width, sourceMeta.Height, s.cfg.Video.VariantBitrates)
	if len(selectedVariants) == 0 {
		return ProcessResponse{}, fmt.Errorf("no HLS variants selected for source dimensions %dx%d", sourceMeta.Width, sourceMeta.Height)
	}

	rendered, err := transcodeHLS(ctx, TranscodeRequest{
		FFmpegBinary:    s.cfg.Video.FFmpegBinary,
		SourcePath:      sourcePath,
		WorkDir:         workDir,
		SegmentLength:   s.cfg.Video.SegmentLength,
		ThumbnailAt:     s.thumbnailOffset(req.ThumbnailTimeSeconds),
		VariantProfiles: selectedVariants,
		Progress: func(progress TranscodeProgress) {
			s.reportProgress(req, s.mapTranscodeProgress(progress))
		},
	})
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("transcode hls: %w", err)
	}

	publishID, err := randomID()
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("create publish id: %w", err)
	}

	stagingPrefix := filepath.ToSlash(filepath.Join(s.cfg.Upload.Prefix, ".staging", videoID, publishID))
	publishPrefix := filepath.ToSlash(filepath.Join(s.cfg.Upload.Prefix, videoID, publishID))
	manifestPath, err := s.createUploadRecoveryManifest(uploadRecoveryManifest{
		VideoID:       videoID,
		StagingPrefix: stagingPrefix,
		PublishPrefix: publishPrefix,
	})
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("create upload recovery manifest: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(manifestPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			log.Printf("failed to remove upload recovery manifest path=%s err=%v", manifestPath, removeErr)
		}
	}()

	s.reportProgress(req, ProcessProgress{
		Phase:   "uploading_staging",
		Percent: 75,
		Message: "Uploading HLS assets to staging storage",
	})
	uploadedObjects, err := s.r2.UploadDirVerified(ctx, rendered.OutputDir, stagingPrefix)
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("upload assets to r2: %w", err)
	}
	if len(uploadedObjects) == 0 {
		return ProcessResponse{}, fmt.Errorf("upload assets to r2: no files were uploaded")
	}

	s.reportProgress(req, ProcessProgress{
		Phase:   "publishing",
		Percent: 90,
		Message: "Publishing staged assets",
	})
	publishedObjects, err := s.r2.CopyPrefixVerified(ctx, stagingPrefix, publishPrefix)
	if err != nil {
		return ProcessResponse{}, fmt.Errorf("publish staged assets to r2: %w", err)
	}
	if len(publishedObjects) == 0 {
		return ProcessResponse{}, fmt.Errorf("publish staged assets to r2: no files were published")
	}
	if err := s.r2.DeletePrefix(ctx, stagingPrefix); err != nil {
		log.Printf("failed to remove staging prefix after publish prefix=%s err=%v", stagingPrefix, err)
	}
	publishedBasePrefix := filepath.ToSlash(filepath.Join(s.cfg.Upload.Prefix, videoID))
	if err := s.r2.DeletePrefixContentsExcept(ctx, publishedBasePrefix, publishPrefix); err != nil {
		log.Printf("failed to remove older published versions prefix=%s keep=%s err=%v", publishedBasePrefix, publishPrefix, err)
	}

	resp := ProcessResponse{
		VideoID:   videoID,
		VideoPath: filepath.ToSlash(filepath.Join(publishPrefix, "master.m3u8")),
		Duration:  int(sourceMeta.DurationSeconds + 0.5),
	}

	if rendered.ThumbnailFile != "" {
		resp.ThumbnailPath = filepath.ToSlash(filepath.Join(publishPrefix, filepath.Base(rendered.ThumbnailFile)))
	}

	resp.Variants = make([]VariantResult, 0, len(rendered.Variants))
	for _, item := range rendered.Variants {
		resp.Variants = append(resp.Variants, VariantResult{
			Name:     item.Profile.Name,
			Playlist: filepath.ToSlash(filepath.Join(publishPrefix, item.RelativePlaylist)),
			Width:    item.Profile.Width,
			Height:   item.Profile.Height,
		})
	}

	if req.CleanupSource {
		s.reportProgress(req, ProcessProgress{
			Phase:   "finalizing",
			Percent: 97,
			Message: "Cleaning source upload",
		})
		if err := os.Remove(sourcePath); err != nil {
			return ProcessResponse{}, fmt.Errorf("cleanup source video after verified upload: %w", err)
		}
		cleanupLocalSource = false
	}

	if req.CleanupSourceObject && strings.TrimSpace(req.SourceObjectKey) != "" {
		if err := s.r2.DeleteObject(ctx, req.SourceObjectKey); err != nil {
			return ProcessResponse{}, fmt.Errorf("cleanup source object after verified upload: %w", err)
		}
	}

	s.reportProgress(req, ProcessProgress{
		Phase:   "completed",
		Percent: 100,
		Message: "Video processing complete",
	})

	return resp, nil
}

func (s *Service) resolveSourcePath(ctx context.Context, req ProcessRequest) (string, bool, error) {
	sourcePath := strings.TrimSpace(req.SourcePath)
	if sourcePath != "" {
		if _, err := os.Stat(sourcePath); err != nil {
			return "", false, fmt.Errorf("%w: sourcePath is not readable: %v", ErrInvalidRequest, err)
		}
		return sourcePath, false, nil
	}

	sourceObjectKey := strings.TrimSpace(req.SourceObjectKey)
	if sourceObjectKey == "" {
		return "", false, fmt.Errorf("%w: sourcePath or sourceObjectKey is required", ErrInvalidRequest)
	}

	extension := filepath.Ext(sourceObjectKey)
	sourceFile, err := s.CreateSourceUploadFile(extension)
	if err != nil {
		return "", false, fmt.Errorf("create source file for object download: %w", err)
	}
	sourcePath = sourceFile.Name()
	if err := sourceFile.Close(); err != nil {
		_ = os.Remove(sourcePath)
		return "", false, fmt.Errorf("close source file for object download: %w", err)
	}

	s.reportProgress(req, ProcessProgress{
		Phase:   "downloading_source",
		Percent: 5,
		Message: "Downloading source upload from storage",
	})
	if err := s.r2.DownloadFile(ctx, sourceObjectKey, sourcePath); err != nil {
		_ = os.Remove(sourcePath)
		return "", false, fmt.Errorf("download source object %s: %w", sourceObjectKey, err)
	}

	return sourcePath, true, nil
}

func (s *Service) thumbnailOffset(explicitSeconds int) int {
	if explicitSeconds > 0 {
		return explicitSeconds
	}
	return int(s.cfg.Video.ThumbnailAt.Seconds())
}

func (s *Service) recoveryDir() string {
	return filepath.Join(s.cfg.Video.WorkingDir, "upload-recovery")
}

func (s *Service) createUploadRecoveryManifest(manifest uploadRecoveryManifest) (string, error) {
	if err := os.MkdirAll(s.recoveryDir(), 0o755); err != nil {
		return "", fmt.Errorf("ensure recovery dir: %w", err)
	}

	manifestID, err := randomID()
	if err != nil {
		return "", fmt.Errorf("create recovery manifest id: %w", err)
	}

	filename := fmt.Sprintf("%s-%s.manifest", manifest.VideoID, manifestID)
	manifestPath := filepath.Join(s.recoveryDir(), filename)
	payload, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal recovery manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, payload, 0o644); err != nil {
		return "", fmt.Errorf("write recovery manifest: %w", err)
	}

	return manifestPath, nil
}

func parseUploadRecoveryManifest(payload []byte) (uploadRecoveryManifest, error) {
	var manifest uploadRecoveryManifest
	if err := json.Unmarshal(payload, &manifest); err == nil {
		return manifest, nil
	}

	legacyPrefix := strings.TrimSpace(string(payload))
	return uploadRecoveryManifest{StagingPrefix: legacyPrefix}, nil
}

func (s *Service) reportProgress(req ProcessRequest, progress ProcessProgress) {
	if req.ProgressCallback == nil {
		return
	}

	progress.Percent = clampPercent(progress.Percent)
	req.ProgressCallback(progress)
}

func (s *Service) mapTranscodeProgress(progress TranscodeProgress) ProcessProgress {
	if progress.TotalVariants <= 0 {
		return ProcessProgress{
			Phase:   "transcoding",
			Percent: 45,
			Message: "Transcoding video",
		}
	}

	completedVariants := progress.CurrentVariant
	if progress.Stage == "start_variant" {
		completedVariants = progress.CurrentVariant - 1
	}
	if completedVariants < 0 {
		completedVariants = 0
	}

	base := 20
	rangeWidth := 45
	percent := base + (completedVariants * rangeWidth / progress.TotalVariants)
	if progress.Stage == "start_variant" {
		percent += rangeWidth / (progress.TotalVariants * 2)
	}

	return ProcessProgress{
		Phase:   "transcoding",
		Percent: percent,
		Message: fmt.Sprintf("Transcoding %s (%d/%d)", progress.VariantName, progress.CurrentVariant, progress.TotalVariants),
	}
}

func clampPercent(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func randomID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
