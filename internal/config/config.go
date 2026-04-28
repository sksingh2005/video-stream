package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Server   ServerConfig
	Jobs     JobConfig
	R2       R2Config
	Video    VideoConfig
	Upload   UploadConfig
	Security SecurityConfig
}

type ServerConfig struct {
	Address string
}

type JobConfig struct {
	WorkerCount      int
	QueueSize        int
	RetentionMinutes int
}

type R2Config struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey string
	BucketName      string
}

type VideoConfig struct {
	FFmpegBinary    string
	FFprobeBinary   string
	WorkingDir      string
	SegmentLength   int
	ThumbnailAt     time.Duration
	VariantBitrates []VariantProfile
}

type VariantProfile struct {
	Name      string
	Width     int
	Height    int
	VideoRate string
	MaxRate   string
	Buffer    string
	AudioRate string
}

type UploadConfig struct {
	Prefix string
}

type SecurityConfig struct {
	PlaybackDomain    string
	TokenSecret       string
	TokenTTLSeconds   int
	AllowedCORSOrigin string
}

func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, err
	}

	cfg := Config{
		Server: ServerConfig{
			Address: getEnv("VIDEO_SERVICE_ADDR", ":8080"),
		},
		Jobs: JobConfig{
			WorkerCount:      getEnvInt("VIDEO_JOB_WORKER_COUNT", 1),
			QueueSize:        getEnvInt("VIDEO_JOB_QUEUE_SIZE", 16),
			RetentionMinutes: getEnvInt("VIDEO_JOB_RETENTION_MINUTES", 120),
		},
		R2: R2Config{
			AccountID:       os.Getenv("R2_ACCOUNT_ID"),
			AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
			BucketName:      os.Getenv("R2_BUCKET_NAME"),
		},
		Video: VideoConfig{
			FFmpegBinary:    getEnv("FFMPEG_BINARY", "ffmpeg"),
			FFprobeBinary:   getEnv("FFPROBE_BINARY", "ffprobe"),
			WorkingDir:      getEnv("VIDEO_WORK_DIR", os.TempDir()),
			SegmentLength:   getEnvInt("HLS_SEGMENT_LENGTH", 6),
			ThumbnailAt:     time.Duration(getEnvInt("THUMBNAIL_AT_SECONDS", 1)) * time.Second,
			VariantBitrates: defaultVariants(),
		},
		Upload: UploadConfig{
			Prefix: strings.Trim(getEnv("VIDEO_UPLOAD_PREFIX", "videos"), "/"),
		},
		Security: SecurityConfig{
			PlaybackDomain:    getEnv("PLAYBACK_DOMAIN", "https://stream.example.com"),
			TokenSecret:       getEnv("PLAYBACK_TOKEN_SECRET", ""),
			TokenTTLSeconds:   getEnvInt("PLAYBACK_TOKEN_TTL_SECONDS", 900),
			AllowedCORSOrigin: getEnv("PLAYBACK_ALLOWED_ORIGIN", "https://app.example.com"),
		},
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) validate() error {
	var missing []string
	if c.R2.AccountID == "" {
		missing = append(missing, "R2_ACCOUNT_ID")
	}
	if c.R2.AccessKeyID == "" {
		missing = append(missing, "R2_ACCESS_KEY_ID")
	}
	if c.R2.SecretAccessKey == "" {
		missing = append(missing, "R2_SECRET_ACCESS_KEY")
	}
	if c.R2.BucketName == "" {
		missing = append(missing, "R2_BUCKET_NAME")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if c.Video.SegmentLength <= 0 {
		return errors.New("HLS_SEGMENT_LENGTH must be positive")
	}
	if c.Jobs.WorkerCount <= 0 {
		return errors.New("VIDEO_JOB_WORKER_COUNT must be positive")
	}
	if c.Jobs.QueueSize <= 0 {
		return errors.New("VIDEO_JOB_QUEUE_SIZE must be positive")
	}
	if c.Jobs.RetentionMinutes <= 0 {
		return errors.New("VIDEO_JOB_RETENTION_MINUTES must be positive")
	}
	if c.Security.TokenTTLSeconds <= 0 {
		return errors.New("PLAYBACK_TOKEN_TTL_SECONDS must be positive")
	}
	return nil
}

func defaultVariants() []VariantProfile {
	return []VariantProfile{
		{Name: "1080p", Width: 1920, Height: 1080, VideoRate: "5000k", MaxRate: "5350k", Buffer: "7500k", AudioRate: "192k"},
		{Name: "720p", Width: 1280, Height: 720, VideoRate: "2800k", MaxRate: "2996k", Buffer: "4200k", AudioRate: "128k"},
		{Name: "480p", Width: 854, Height: 480, VideoRate: "1400k", MaxRate: "1498k", Buffer: "2100k", AudioRate: "128k"},
		{Name: "360p", Width: 640, Height: 360, VideoRate: "800k", MaxRate: "856k", Buffer: "1200k", AudioRate: "96k"},
	}
}

func getEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func getEnvInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
