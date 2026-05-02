package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sksingh2005/video-stream/internal/video"
)

type Handler struct {
	service *video.Service
	jobs    *video.JobManager
}

func NewHandler(service *video.Service, jobs *video.JobManager) http.Handler {
	h := &Handler{service: service, jobs: jobs}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.handleHealth)
	mux.HandleFunc("/api/v1/videos/process", h.handleProcessVideo)
	mux.HandleFunc("/api/v1/videos/upload", h.handleUploadVideo)
	mux.HandleFunc("/api/v1/video-jobs/", h.handleVideoJob)
	return withJSON(mux)
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"timestamp": time.Now().UTC(),
	})
}

func (h *Handler) handleProcessVideo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req video.ProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err)
		return
	}

	resp, err := h.service.ProcessAndUpload(r.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, video.ErrInvalidRequest) {
			status = http.StatusBadRequest
		}
		writeError(w, status, "video_processing_failed", err)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleUploadVideo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_multipart", err)
		return
	}

	videoID := strings.TrimSpace(r.FormValue("videoId"))
	if videoID == "" {
		writeError(w, http.StatusBadRequest, "missing_video_id", fmt.Errorf("videoId is required"))
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing_file", err)
		return
	}
	defer file.Close()

	extension := filepath.Ext(header.Filename)
	if extension == "" {
		extension = ".mp4"
	}

	sourceFile, err := h.service.CreateSourceUploadFile(extension)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create_source_file_failed", err)
		return
	}
	sourcePath := sourceFile.Name()

	if _, err := io.Copy(sourceFile, file); err != nil {
		sourceFile.Close()
		_ = os.Remove(sourcePath)
		writeError(w, http.StatusInternalServerError, "store_upload_failed", err)
		return
	}
	if err := sourceFile.Close(); err != nil {
		_ = os.Remove(sourcePath)
		writeError(w, http.StatusInternalServerError, "store_upload_failed", err)
		return
	}

	thumbnailTimeSeconds := 0
	if raw := strings.TrimSpace(r.FormValue("thumbnailTimeSeconds")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_thumbnail_time", err)
			return
		}
		thumbnailTimeSeconds = parsed
	}

	job, err := h.jobs.Enqueue(video.ProcessRequest{
		VideoID:              videoID,
		SourcePath:           sourcePath,
		ThumbnailTimeSeconds: thumbnailTimeSeconds,
		CleanupSource:        true,
	}, true)
	if err != nil {
		_ = os.Remove(sourcePath)
		switch {
		case errors.Is(err, video.ErrInvalidRequest):
			writeError(w, http.StatusBadRequest, "invalid_video_job", err)
		case errors.Is(err, video.ErrVideoAlreadyProcessing):
			writeError(w, http.StatusConflict, "video_job_already_active", err)
		case errors.Is(err, video.ErrJobQueueFull):
			writeError(w, http.StatusServiceUnavailable, "video_job_queue_full", err)
		default:
			writeError(w, http.StatusInternalServerError, "video_job_enqueue_failed", err)
		}
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"job": job,
	})
}

func (h *Handler) handleVideoJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobID := strings.TrimPrefix(r.URL.Path, "/api/v1/video-jobs/")
	jobID = strings.TrimSpace(jobID)
	if jobID == "" || strings.Contains(jobID, "/") {
		writeError(w, http.StatusNotFound, "job_not_found", fmt.Errorf("video job not found"))
		return
	}

	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "job_not_found", fmt.Errorf("video job not found"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"job": job,
	})
}

func withJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code string, err error) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": err.Error(),
		},
	})
}
