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
	"sync"
	"time"

	"github.com/sksingh2005/video-stream/internal/config"
)

var ErrJobQueueFull = errors.New("job queue is full")
var ErrVideoAlreadyProcessing = errors.New("video already has an active processing job")

type JobStatus string

const (
	JobStatusQueued     JobStatus = "queued"
	JobStatusProcessing JobStatus = "processing"
	JobStatusSucceeded  JobStatus = "succeeded"
	JobStatusFailed     JobStatus = "failed"
)

type ProcessJob struct {
	ID          string           `json:"jobId"`
	Status      JobStatus        `json:"status"`
	VideoID     string           `json:"videoId"`
	Progress    ProcessProgress  `json:"progress"`
	Result      *ProcessResponse `json:"result,omitempty"`
	Error       string           `json:"error,omitempty"`
	CreatedAt   time.Time        `json:"createdAt"`
	StartedAt   *time.Time       `json:"startedAt,omitempty"`
	CompletedAt *time.Time       `json:"completedAt,omitempty"`
}

type jobRequest struct {
	ProcessRequest
	OwnsSource bool
}

type jobState struct {
	ProcessJob
	request jobRequest
}

type JobManager struct {
	service   *Service
	queue     chan string
	retention time.Duration
	stateDir  string

	mu            sync.RWMutex
	jobs          map[string]*jobState
	activeVideoID map[string]string
}

func NewJobManager(cfg config.JobConfig, service *Service) *JobManager {
	return &JobManager{
		service:       service,
		queue:         make(chan string, cfg.QueueSize),
		retention:     time.Duration(cfg.RetentionMinutes) * time.Minute,
		stateDir:      cfg.StateDir,
		jobs:          make(map[string]*jobState),
		activeVideoID: make(map[string]string),
	}
}

func (m *JobManager) Start(ctx context.Context, workerCount int) {
	if err := m.loadPersistedJobs(); err != nil {
		log.Printf("failed to load persisted jobs: %v", err)
	}
	m.recoverPendingJobs()
	for i := 0; i < workerCount; i++ {
		go m.worker(ctx)
	}
}

func (m *JobManager) Enqueue(req ProcessRequest, ownsSource bool) (ProcessJob, error) {
	m.pruneExpired()

	videoID, err := sanitizeVideoID(req.VideoID)
	if err != nil {
		return ProcessJob{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	req.VideoID = videoID

	jobID, err := newJobID()
	if err != nil {
		return ProcessJob{}, fmt.Errorf("create job id: %w", err)
	}

	job := &jobState{
		ProcessJob: ProcessJob{
			ID:      jobID,
			Status:  JobStatusQueued,
			VideoID: req.VideoID,
			Progress: ProcessProgress{
				Phase:   "queued",
				Percent: 0,
				Message: "Waiting for an available worker",
			},
			CreatedAt: time.Now().UTC(),
		},
		request: jobRequest{
			ProcessRequest: req,
			OwnsSource:     ownsSource,
		},
	}

	m.mu.Lock()
	if activeJobID, exists := m.activeVideoID[videoID]; exists {
		m.mu.Unlock()
		log.Printf("rejected duplicate video job videoId=%s activeJobId=%s", videoID, activeJobID)
		if ownsSource {
			_ = os.Remove(req.SourcePath)
		}
		return ProcessJob{}, fmt.Errorf("%w: videoId %q is already running in job %s", ErrVideoAlreadyProcessing, videoID, activeJobID)
	}
	m.jobs[jobID] = job
	m.activeVideoID[videoID] = jobID
	if err := m.persistJobLocked(job); err != nil {
		delete(m.jobs, jobID)
		delete(m.activeVideoID, videoID)
		m.mu.Unlock()
		if ownsSource {
			_ = os.Remove(req.SourcePath)
		}
		return ProcessJob{}, fmt.Errorf("persist queued job: %w", err)
	}
	m.mu.Unlock()

	select {
	case m.queue <- jobID:
		log.Printf("queued video job jobId=%s videoId=%s", jobID, req.VideoID)
		return job.snapshot(), nil
	default:
		m.mu.Lock()
		delete(m.jobs, jobID)
		delete(m.activeVideoID, videoID)
		_ = m.deletePersistedJobLocked(jobID)
		m.mu.Unlock()
		log.Printf("rejected video job queue full jobId=%s videoId=%s", jobID, req.VideoID)
		if ownsSource {
			_ = os.Remove(req.SourcePath)
		}
		return ProcessJob{}, ErrJobQueueFull
	}
}

func (m *JobManager) Get(jobID string) (ProcessJob, bool) {
	m.pruneExpired()

	m.mu.RLock()
	job, ok := m.jobs[jobID]
	m.mu.RUnlock()
	if !ok {
		return ProcessJob{}, false
	}
	return job.snapshot(), true
}

func (m *JobManager) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case jobID := <-m.queue:
			m.runJob(ctx, jobID)
		}
	}
}

func (m *JobManager) runJob(ctx context.Context, jobID string) {
	queued := time.Now().UTC()

	m.mu.Lock()
	job, ok := m.jobs[jobID]
	if !ok {
		m.mu.Unlock()
		return
	}
	job.Status = JobStatusProcessing
	job.StartedAt = &queued
	queueWait := queued.Sub(job.CreatedAt)
	job.Progress = ProcessProgress{
		Phase:   "starting",
		Percent: 5,
		Message: "Starting video processing",
	}
	job.request.ProgressCallback = m.progressReporter(jobID)
	if err := m.persistJobLocked(job); err != nil {
		log.Printf("failed to persist processing job jobId=%s err=%v", jobID, err)
	}
	m.mu.Unlock()

	log.Printf("started video job jobId=%s videoId=%s queueWaitMs=%d", jobID, job.VideoID, queueWait.Milliseconds())
	result, err := m.service.ProcessAndUpload(ctx, job.request.ProcessRequest)

	completed := time.Now().UTC()

	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.jobs[jobID]
	if !ok {
		return
	}

	current.CompletedAt = &completed
	if err != nil {
		current.Status = JobStatusFailed
		current.Error = err.Error()
		current.Progress = ProcessProgress{
			Phase:   "failed",
			Percent: current.Progress.Percent,
			Message: err.Error(),
		}
		delete(m.activeVideoID, current.VideoID)
		if persistErr := m.persistJobLocked(current); persistErr != nil {
			log.Printf("failed to persist failed job jobId=%s err=%v", jobID, persistErr)
		}
		runDuration := completed.Sub(queued)
		log.Printf("failed video job jobId=%s videoId=%s durationMs=%d error=%q", jobID, current.VideoID, runDuration.Milliseconds(), err.Error())
		if current.request.OwnsSource {
			_ = os.Remove(current.request.SourcePath)
		}
		return
	}

	current.Status = JobStatusSucceeded
	current.Result = &result
	current.Error = ""
	current.Progress = ProcessProgress{
		Phase:   "completed",
		Percent: 100,
		Message: "Video processing complete",
	}
	delete(m.activeVideoID, current.VideoID)
	if persistErr := m.persistJobLocked(current); persistErr != nil {
		log.Printf("failed to persist completed job jobId=%s err=%v", jobID, persistErr)
	}
	runDuration := completed.Sub(queued)
	log.Printf("completed video job jobId=%s videoId=%s durationMs=%d output=%s", jobID, current.VideoID, runDuration.Milliseconds(), result.VideoPath)
}

func (m *JobManager) pruneExpired() {
	cutoff := time.Now().UTC().Add(-m.retention)

	m.mu.Lock()
	defer m.mu.Unlock()

	for jobID, job := range m.jobs {
		if job.CompletedAt != nil && job.CompletedAt.Before(cutoff) {
			delete(m.jobs, jobID)
			if err := m.deletePersistedJobLocked(jobID); err != nil {
				log.Printf("failed to delete persisted expired job jobId=%s err=%v", jobID, err)
			}
		}
	}
}

func (j *jobState) snapshot() ProcessJob {
	copy := j.ProcessJob
	return copy
}

func (m *JobManager) progressReporter(jobID string) func(ProcessProgress) {
	return func(progress ProcessProgress) {
		m.mu.Lock()
		defer m.mu.Unlock()

		job, ok := m.jobs[jobID]
		if !ok {
			return
		}
		if job.Status == JobStatusSucceeded || job.Status == JobStatusFailed {
			return
		}

		job.Progress = progress
		if err := m.persistJobLocked(job); err != nil {
			log.Printf("failed to persist job progress jobId=%s err=%v", jobID, err)
		}
	}
}

type persistedJob struct {
	ProcessJob ProcessJob `json:"processJob"`
	Request    jobRequest `json:"request"`
}

func (m *JobManager) loadPersistedJobs() error {
	if err := os.MkdirAll(m.stateDir, 0o755); err != nil {
		return fmt.Errorf("ensure job state dir: %w", err)
	}

	entries, err := os.ReadDir(m.stateDir)
	if err != nil {
		return fmt.Errorf("read job state dir: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		payload, err := os.ReadFile(filepath.Join(m.stateDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("read persisted job %s: %w", entry.Name(), err)
		}

		var persisted persistedJob
		if err := json.Unmarshal(payload, &persisted); err != nil {
			return fmt.Errorf("parse persisted job %s: %w", entry.Name(), err)
		}

		if persisted.ProcessJob.ID == "" {
			continue
		}

		job := &jobState{
			ProcessJob: persisted.ProcessJob,
			request:    persisted.Request,
		}
		m.jobs[job.ID] = job
		if job.Status == JobStatusQueued || job.Status == JobStatusProcessing {
			m.activeVideoID[job.VideoID] = job.ID
		}
	}

	return nil
}

func (m *JobManager) recoverPendingJobs() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, job := range m.jobs {
		if job.Status != JobStatusQueued && job.Status != JobStatusProcessing {
			continue
		}

		if job.request.OwnsSource {
			if _, err := os.Stat(job.request.SourcePath); err != nil {
				now := time.Now().UTC()
				job.Status = JobStatusFailed
				job.Error = fmt.Sprintf("source file missing after restart: %v", err)
				job.Progress = ProcessProgress{
					Phase:   "failed",
					Percent: job.Progress.Percent,
					Message: job.Error,
				}
				job.CompletedAt = &now
				delete(m.activeVideoID, job.VideoID)
				if persistErr := m.persistJobLocked(job); persistErr != nil {
					log.Printf("failed to persist missing-source job jobId=%s err=%v", job.ID, persistErr)
				}
				continue
			}
		}

		job.Status = JobStatusQueued
		job.StartedAt = nil
		job.CompletedAt = nil
		job.Error = ""
		job.Result = nil
		job.Progress = ProcessProgress{
			Phase:   "queued",
			Percent: 0,
			Message: "Recovered after processor restart",
		}
		if persistErr := m.persistJobLocked(job); persistErr != nil {
			log.Printf("failed to persist recovered job jobId=%s err=%v", job.ID, persistErr)
			continue
		}

		select {
		case m.queue <- job.ID:
			log.Printf("recovered persisted video job jobId=%s videoId=%s", job.ID, job.VideoID)
		default:
			job.Status = JobStatusFailed
			job.Error = "job queue is full during restart recovery"
			job.Progress = ProcessProgress{
				Phase:   "failed",
				Percent: 0,
				Message: job.Error,
			}
			now := time.Now().UTC()
			job.CompletedAt = &now
			delete(m.activeVideoID, job.VideoID)
			if persistErr := m.persistJobLocked(job); persistErr != nil {
				log.Printf("failed to persist recovery queue-full job jobId=%s err=%v", job.ID, persistErr)
			}
		}
	}
}

func (m *JobManager) persistJobLocked(job *jobState) error {
	if err := os.MkdirAll(m.stateDir, 0o755); err != nil {
		return fmt.Errorf("ensure job state dir: %w", err)
	}

	payload, err := json.MarshalIndent(persistedJob{
		ProcessJob: job.ProcessJob,
		Request:    job.request,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal persisted job: %w", err)
	}

	path := filepath.Join(m.stateDir, job.ID+".json")
	return os.WriteFile(path, payload, 0o644)
}

func (m *JobManager) deletePersistedJobLocked(jobID string) error {
	path := filepath.Join(m.stateDir, jobID+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func newJobID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
