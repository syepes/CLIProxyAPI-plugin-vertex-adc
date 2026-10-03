package provider

import (
	"context"
	"sync"
	"time"

	"cliproxyapi-vertex-adc/internal/transport"
)

const providerID = "vertex-adc"

type Service struct {
	host      transport.Host
	now       func() time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	workMu    sync.Mutex
	closed    bool
	wg        sync.WaitGroup
	startOnce sync.Once

	configMu sync.RWMutex
	config   Config

	authOnce sync.Once
	adc      *adcCredentials
	adcErr   error

	modelMu       sync.Mutex
	modelCache    *modelCacheEntry
	modelInflight *modelFlight
}

func New(host transport.Host) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{host: host, now: time.Now, ctx: ctx, cancel: cancel, config: DefaultConfig()}
}

func (s *Service) Context() context.Context { return s.ctx }

// spawn registers every background worker before shutdown may wait for it.
func (s *Service) spawn(f func()) bool {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if recover() != nil {
				s.cancel()
			}
		}()
		f()
	}()
	return true
}

func (s *Service) Configure(raw []byte) error {
	cfg, err := ParseConfig(raw)
	if err != nil {
		return err
	}
	s.configMu.Lock()
	s.config = cfg
	s.configMu.Unlock()
	return nil
}

func (s *Service) Config() Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	c := s.config
	c.Models = append([]ModelConfig(nil), c.Models...)
	c.ModelsExcluded = append([]string(nil), c.ModelsExcluded...)
	c.Publishers = append([]string(nil), c.Publishers...)
	return c
}

func (s *Service) Shutdown() {
	s.workMu.Lock()
	s.closed = true
	s.cancel()
	s.workMu.Unlock()
	s.wg.Wait()
	s.modelMu.Lock()
	s.modelCache = nil
	s.modelInflight = nil
	s.modelMu.Unlock()
}

// Start warms the model catalog and keeps it fresh so model.static stays current
// without blocking every host call on a network round trip.
func (s *Service) Start() {
	s.startOnce.Do(func() {
		s.spawn(func() {
			// Warm once, then refresh on the cache TTL cadence.
			s.refreshCatalog()
			interval := s.Config().modelCacheTTL()
			if interval <= 0 {
				interval = 5 * time.Minute
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
					s.refreshCatalog()
				}
			}
		})
	})
}

func (s *Service) refreshCatalog() {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	_, _ = s.catalog(ctx, true)
}
