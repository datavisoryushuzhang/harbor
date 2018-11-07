package provider

import (
	"github.com/goharbor/harbor/src/distribution/models"
)

// Factory is responsible to create a new driver based on the metadata.
type Factory func(meta *models.Metadata) (Driver, error)

// DragonflyFactory creates dragonfly driver
func DragonflyFactory(meta *models.Metadata) (Driver, error) {
	return &DragonflyDriver{meta}, nil
}
