package store

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/streammux/streammux/internal/domain/model"
)

type MemoryStore struct {
	mu        sync.RWMutex
	jobs      map[string]*model.MuxJob
	times     map[string]time.Time
	ttl       time.Duration
	onDelete  func(*model.MuxJob)
}

func NewMemoryStore(ttl time.Duration) *MemoryStore {
	s := &MemoryStore{
		jobs:  make(map[string]*model.MuxJob),
		times: make(map[string]time.Time),
		ttl:   ttl,
	}
	go s.cleanupLoop()
	return s
}

// SetOnDelete registers a callback called when a job is deleted (by TTL or
// explicit Delete). Used to clean up HLS temp directories.
func (s *MemoryStore) SetOnDelete(fn func(*model.MuxJob)) {
	s.mu.Lock()
	s.onDelete = fn
	s.mu.Unlock()
}

func (s *MemoryStore) Save(job *model.MuxJob) string {
	if job.ID == "" {
		job.ID = generateID()
	}
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.times[job.ID] = time.Now()
	s.mu.Unlock()
	return job.ID
}

func (s *MemoryStore) Get(id string) (*model.MuxJob, bool) {
	s.mu.RLock()
	job, ok := s.jobs[id]
	s.mu.RUnlock()
	if ok {
		s.mu.Lock()
		s.times[id] = time.Now()
		s.mu.Unlock()
	}
	return job, ok
}

func (s *MemoryStore) Delete(id string) {
	s.mu.Lock()
	if job, ok := s.jobs[id]; ok {
		if s.onDelete != nil {
			s.onDelete(job)
		}
		delete(s.jobs, id)
		delete(s.times, id)
	}
	s.mu.Unlock()
}

func (s *MemoryStore) Cleanup() {
	s.mu.Lock()
	now := time.Now()
	for id, t := range s.times {
		if now.Sub(t) > s.ttl {
			if job, ok := s.jobs[id]; ok && s.onDelete != nil {
				s.onDelete(job)
			}
			delete(s.jobs, id)
			delete(s.times, id)
		}
	}
	s.mu.Unlock()
}

func (s *MemoryStore) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.Cleanup()
	}
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
