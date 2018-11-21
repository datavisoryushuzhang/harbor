package distribution

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goharbor/harbor/src/common/utils/log"

	"github.com/goharbor/harbor/src/distribution/models"
	"github.com/goharbor/harbor/src/distribution/provider"
	"github.com/goharbor/harbor/src/distribution/storage/history"
	"github.com/goharbor/harbor/src/distribution/storage/instance"
)

const (
	healthCheckLoopInterval = 1 * time.Minute
	progressUpdateInterval  = 5 * time.Second
	qSize                   = 1024
)

type progressItem struct {
	instanceID string
	taskID     string
}

// Monitor the instance health and distribution status.
// Update the related status flag if needed.
type Monitor struct {
	// Cancellable context
	context context.Context

	// For history
	hStore history.Storage

	// For instance
	iStore instance.Storage

	// Queue for history updating
	q chan *progressItem
}

// NewMonitor is constructor of Monitor
func NewMonitor(ctx context.Context, iStorage instance.Storage, hStorage history.Storage) *Monitor {
	return &Monitor{
		context: ctx,
		hStore:  hStorage,
		iStore:  iStorage,
		q:       make(chan *progressItem, qSize),
	}
}

// Start the loops
func (m *Monitor) Start() {
	// Start instance health check loop
	tk := time.NewTicker(healthCheckLoopInterval)
	defer tk.Stop()

	go func() {
		defer func() {
			log.Info("Monitor health check loop exit")
		}()
		for {
			select {
			case <-tk.C:
				m.healthLoop()
			case <-m.context.Done():
				return
			}
		}
	}()

	// Start progress update loop
	go func() {
		defer func() {
			log.Info("Monitor progress check loop exit")
		}()

		for {
			select {
			case item := <-m.q:
				go func() {
					if done, err := m.checkTaskProgress(item.instanceID, item.taskID); err != nil {
						log.Errorf("update progress error: %s", err)
					} else {
						if !done {
							// Keep on checking
							// put back
							// non blocking
							go func() {
								<-time.After(progressUpdateInterval)
								m.q <- item
							}()
						}
					}
				}()
			case <-m.context.Done():
				return
			}
		}
	}()
}

// WatchProgress watches the preheating task progress
// non blocking
func (m *Monitor) WatchProgress(instanceID, taskID string) {
	go func() {
		m.q <- &progressItem{
			instanceID: instanceID,
			taskID:     taskID,
		}
	}()
}

func (m *Monitor) healthLoop() {
	all, err := m.iStore.List(nil)
	if err != nil {
		log.Errorf("health loop error: %s", err)
		return
	}

	for _, inst := range all {
		go func(inst *models.Metadata) {
			if err := m.checkInstanceHealth(inst); err != nil {
				log.Errorf("check instance health error: %s", err)
			}
		}(inst)
	}
}

func (m *Monitor) checkTaskProgress(instID string, taskID string) (bool, error) {
	meta, err := m.iStore.Get(instID)
	if err != nil {
		return false, err
	}

	p, err := getProvider(meta)
	if err != nil {
		return false, err
	}

	pStatus, err := p.CheckProgress(taskID)
	if err != nil {
		return false, err
	}

	trackStatus := models.TrackStatus(pStatus.Status)
	// Update history record
	if err := m.hStore.UpdateStatus(taskID, trackStatus); err != nil {
		return false, err
	}

	done := trackStatus.Success() || trackStatus.Fail()

	return done, nil
}

func (m *Monitor) checkInstanceHealth(inst *models.Metadata) error {
	p, err := getProvider(inst)
	if err != nil {
		return err
	}

	status, err := p.GetHealth()
	if err != nil {
		return err
	}

	meta, err := m.iStore.Get(inst.ID)
	if err != nil {
		return err
	}

	meta.Status = status.Status

	return m.iStore.Update(meta)
}

func getProvider(inst *models.Metadata) (provider.Driver, error) {
	if inst == nil {
		return nil, errors.New("nil instance")
	}

	factory, ok := provider.GetProvider(inst.Provider)
	if !ok {
		return nil, fmt.Errorf("no provider with ID %s existing", inst.Provider)
	}

	p, err := factory(inst)
	if err != nil {
		return nil, err
	}

	return p, nil
}
