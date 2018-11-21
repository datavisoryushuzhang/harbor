package models

const (
	// PreheatingImageTypeImage defines the 'image' type of preheating images
	PreheatingImageTypeImage = "image"
	// PreheatingStatusPending means the preheating is waiting for starting
	PreheatingStatusPending = "PENDING"
	// PreheatingStatusRunning means the preheating is ongoing
	PreheatingStatusRunning = "RUNNING"
	// PreheatingStatusSuccess means the preheating is success
	PreheatingStatusSuccess = "SUCCESS"
	// PreheatingStatusFail means the preheating is failed
	PreheatingStatusFail = "FAIL"
)

// Metadata represents the basic info of one working node for the specified provider.
type Metadata struct {
	// Unique ID
	ID string

	// Based on which driver, identified by ID
	Provider string

	// The service endpoint of this instance
	Endpoint string

	// The authentication way supported
	AuthMode string `json:"auth_mode,omitempty"`

	// The auth credential data if exists
	AuthData map[string]string `json:"auth_data,omitempty"`

	// The health status
	Status string `json:"status,omitempty"`

	// Whether the instance is activated or not
	Enabled bool

	// The timestamp of instance setting up
	SetupTimestamp int64 `json:"setup_timestamp,omitempty"`

	// Append more described data if needed
	Extensions map[string]string `json:"extensions,omitempty"`
}

// HistoryRecord represents one record of the image preheating process.
type HistoryRecord struct {
	TaskID    string `json:"task_id"` // mapping to the provider task ID
	Image     string
	Timestamp int64
	Status    string
	Provider  string
	Instance  string
}

// QueryParam is a collection of parameters for querying preheating history records.
type QueryParam struct {
	Page      uint
	PageSize  uint
	Keyword   string
	Additions map[string]interface{}
}
