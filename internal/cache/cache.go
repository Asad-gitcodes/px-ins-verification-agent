package cache

import (
	"fmt"
	"sync"
	"time"

	"insurance-benefit-agent-go/internal/models"
)

type Cache struct {
	mu       sync.RWMutex
	snapshot *WorkSnapshot
}

type WorkSnapshot struct {
	CreatedAt     time.Time             `json:"createdAt"`
	ExpiresAt     time.Time             `json:"expiresAt"`
	OfficeKey     string                `json:"officeKey"`
	UserID        int                   `json:"userId"`
	ConfigAPIURL  string                `json:"configApiUrl"`
	ScraperConfig *models.ScraperConfig `json:"scraperConfig"`
	Payers        []models.Payer        `json:"payers"`
}

func New() *Cache {
	return &Cache{}
}

func (c *Cache) SaveSnapshot(snapshot *WorkSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("snapshot is nil")
	}
	c.mu.Lock()
	c.snapshot = snapshot
	c.mu.Unlock()
	return nil
}

func (c *Cache) LoadValidSnapshot(officeKey string) (*WorkSnapshot, error) {
	c.mu.RLock()
	snapshot := c.snapshot
	c.mu.RUnlock()

	if snapshot == nil {
		return nil, fmt.Errorf("no snapshot in memory")
	}
	if snapshot.OfficeKey != officeKey {
		return nil, fmt.Errorf("snapshot officeKey mismatch: got %s", snapshot.OfficeKey)
	}
	if snapshot.CreatedAt.IsZero() {
		return nil, fmt.Errorf("snapshot createdAt is missing")
	}
	if len(snapshot.Payers) == 0 {
		return nil, fmt.Errorf("snapshot has no payers")
	}
	return snapshot, nil
}
