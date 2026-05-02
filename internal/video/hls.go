package video

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sksingh2005/video-stream/internal/config"
)

type TranscodeRequest struct {
	FFmpegBinary    string
	SourcePath      string
	WorkDir         string
	SegmentLength   int
	ThumbnailAt     int
	VariantProfiles []config.VariantProfile
	Progress        func(TranscodeProgress)
}

type TranscodeResult struct {
	OutputDir     string
	ThumbnailFile string
	Variants      []RenderedVariant
}

type RenderedVariant struct {
	Profile          config.VariantProfile
	RelativePlaylist string
}

type TranscodeProgress struct {
	CurrentVariant int
	TotalVariants  int
	VariantName    string
	Stage          string
}

func SelectVariants(sourceWidth, sourceHeight int, variants []config.VariantProfile) []config.VariantProfile {
	selected := make([]config.VariantProfile, 0, len(variants))
	for _, variant := range variants {
		if variant.Width <= sourceWidth && variant.Height <= sourceHeight {
			selected = append(selected, variant)
		}
	}
	if len(selected) == 0 && len(variants) > 0 {
		fallback := variants[len(variants)-1]
		fallback.Width = sourceWidth
		fallback.Height = sourceHeight
		selected = append(selected, fallback)
	}
	return selected
}

func TranscodeHLS(ctx context.Context, req TranscodeRequest) (TranscodeResult, error) {
	outputDir := filepath.Join(req.WorkDir, "output")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return TranscodeResult{}, fmt.Errorf("create output dir: %w", err)
	}

	result := TranscodeResult{
		OutputDir: outputDir,
		Variants:  make([]RenderedVariant, 0, len(req.VariantProfiles)),
	}

	for _, variant := range req.VariantProfiles {
		if req.Progress != nil {
			req.Progress(TranscodeProgress{
				CurrentVariant: len(result.Variants) + 1,
				TotalVariants:  len(req.VariantProfiles),
				VariantName:    variant.Name,
				Stage:          "start_variant",
			})
		}

		variantDir := filepath.Join(outputDir, variant.Name)
		if err := os.MkdirAll(variantDir, 0o755); err != nil {
			return TranscodeResult{}, fmt.Errorf("create variant dir: %w", err)
		}

		playlist := filepath.Join(variantDir, "index.m3u8")
		segmentPattern := filepath.Join(variantDir, "segment_%03d.ts")
		args := []string{
			"-y",
			"-i", req.SourcePath,
			"-vf", fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2", variant.Width, variant.Height, variant.Width, variant.Height),
			"-c:a", "aac",
			"-ar", "48000",
			"-b:a", variant.AudioRate,
			"-c:v", "h264",
			"-profile:v", "main",
			"-crf", "20",
			"-g", "48",
			"-keyint_min", "48",
			"-sc_threshold", "0",
			"-b:v", variant.VideoRate,
			"-maxrate", variant.MaxRate,
			"-bufsize", variant.Buffer,
			"-hls_time", fmt.Sprintf("%d", req.SegmentLength),
			"-hls_playlist_type", "vod",
			"-hls_segment_filename", segmentPattern,
			"-f", "hls",
			playlist,
		}

		cmd := exec.CommandContext(ctx, req.FFmpegBinary, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return TranscodeResult{}, fmt.Errorf("ffmpeg %s: %w (%s)", variant.Name, err, strings.TrimSpace(stderr.String()))
		}

		result.Variants = append(result.Variants, RenderedVariant{
			Profile:          variant,
			RelativePlaylist: filepath.ToSlash(filepath.Join(variant.Name, "index.m3u8")),
		})
		if req.Progress != nil {
			req.Progress(TranscodeProgress{
				CurrentVariant: len(result.Variants),
				TotalVariants:  len(req.VariantProfiles),
				VariantName:    variant.Name,
				Stage:          "complete_variant",
			})
		}
	}

	if err := writeMasterPlaylist(outputDir, result.Variants); err != nil {
		return TranscodeResult{}, fmt.Errorf("write master playlist: %w", err)
	}

	if req.ThumbnailAt >= 0 {
		thumbnailFile := filepath.Join(outputDir, "thumb.jpg")
		if err := generateThumbnail(ctx, req.FFmpegBinary, req.SourcePath, thumbnailFile, req.ThumbnailAt); err == nil {
			result.ThumbnailFile = thumbnailFile
		}
	}

	return result, nil
}

func writeMasterPlaylist(outputDir string, variants []RenderedVariant) error {
	var builder strings.Builder
	builder.WriteString("#EXTM3U\n")
	builder.WriteString("#EXT-X-VERSION:3\n")

	for _, variant := range variants {
		bandwidth := estimateBandwidth(variant.Profile.VideoRate, variant.Profile.AudioRate)
		builder.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d\n", bandwidth, variant.Profile.Width, variant.Profile.Height))
		builder.WriteString(variant.RelativePlaylist)
		builder.WriteString("\n")
	}

	return os.WriteFile(filepath.Join(outputDir, "master.m3u8"), []byte(builder.String()), 0o644)
}

func generateThumbnail(ctx context.Context, ffmpegBinary, sourcePath, thumbnailFile string, second int) error {
	cmd := exec.CommandContext(
		ctx,
		ffmpegBinary,
		"-y",
		"-ss", fmt.Sprintf("%d", second),
		"-i", sourcePath,
		"-frames:v", "1",
		"-q:v", "2",
		thumbnailFile,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("generate thumbnail: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func estimateBandwidth(videoRate, audioRate string) int {
	parse := func(raw string) int {
		raw = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), "k")
		value := 0
		fmt.Sscanf(raw, "%d", &value)
		return value * 1000
	}
	return parse(videoRate) + parse(audioRate)
}
