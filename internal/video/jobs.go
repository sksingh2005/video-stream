package video

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
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

	mu            sync.RWMutex
	jobs          map[string]*jobState
	activeVideoID map[string]string
}

func NewJobManager(cfg config.JobConfig, service *Service) *JobManager {
	return &JobManager{
		service:       service,
		queue:         make(chan string, cfg.QueueSize),
		retention:     time.Duration(cfg.RetentionMinutes) * time.Minute,
		jobs:          make(map[string]*jobState),
		activeVideoID: make(map[string]string),
	}
}

func (m *JobManager) Start(ctx context.Context, workerCount int) {
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
			ID:        jobID,
			Status:    JobStatusQueued,
			VideoID:   req.VideoID,
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
	m.mu.Unlock()

	select {
	case m.queue <- jobID:
		log.Printf("queued video job jobId=%s videoId=%s", jobID, req.VideoID)
		return job.snapshot(), nil
	default:
		m.mu.Lock()
		delete(m.jobs, jobID)
		delete(m.activeVideoID, videoID)
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
		delete(m.activeVideoID, current.VideoID)
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
	delete(m.activeVideoID, current.VideoID)
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
		}
	}
}

func (j *jobState) snapshot() ProcessJob {
	copy := j.ProcessJob
	return copy
}

func newJobID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
