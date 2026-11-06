// Package report defines the oos report (the JSON contract) and renders it
// as JSON or text. It has no knowledge of how the data was collected.
package report

import (
	"encoding/json"
	"io"
	"time"
)

// SchemaVersion of the JSON report. Bump on incompatible changes.
const SchemaVersion = 1

// Report is the full result of one scan. Its JSON form is the contract
// other tools (and agents) read.
type Report struct {
	SchemaVersion int        `json:"schema_version"`
	Scan          Scan       `json:"scan"`
	Regions       []Region   `json:"regions"`
	Sources       []Source   `json:"sources"`
	Summary       Summary    `json:"summary"`
	Findings      []Finding  `json:"findings"`
	Warnings      []Warning  `json:"warnings"`
	Cost          *CostBlock `json:"cost"` // nil until cost is implemented or when --no-cost
}

// Scan describes the run.
type Scan struct {
	Time      time.Time `json:"time"`
	Version   string    `json:"oos_version"`
	Partition string    `json:"partition"`
	AccountID string    `json:"account_id"`
	Regions   []string  `json:"regions"`
	// Complete is false when any source or region failed or was partial.
	Complete bool `json:"complete"`
	// StateComplete and DiscoveryComplete say which part was incomplete.
	StateComplete     bool `json:"state_complete"`
	DiscoveryComplete bool `json:"discovery_complete"`
}

// Region is the discovery result for one region.
type Region struct {
	Region        string `json:"region"`
	Status        string `json:"status"` // ok, failed
	ErrorCategory string `json:"error_category,omitempty"`
	Error         string `json:"error,omitempty"` // fixed message for the category
	InstanceCount int    `json:"instance_count"`
}

// Source is one Terraform state source.
type Source struct {
	Name          string    `json:"name"`
	Kind          string    `json:"kind"` // directory, file
	Workspace     string    `json:"workspace"`
	Status        string    `json:"status"` // ok, partial, failed
	ErrorCategory string    `json:"error_category,omitempty"`
	Error         string    `json:"error,omitempty"` // fixed message for the category
	Lineage       *string   `json:"lineage"`         // null when the state could not be read
	Serial        *int64    `json:"serial"`          // null when the state could not be read
	IdentityCount int       `json:"identity_count"`
	Problems      []Problem `json:"problems,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

// Problem is an aws_db_instance object in state that could not be used.
type Problem struct {
	Address string `json:"address,omitempty"`
	Kind    string `json:"kind"`
}

// Summary counts findings by status.
type Summary struct {
	Tracked        int `json:"tracked"`
	NoMatch        int `json:"no_match"`
	UnableToVerify int `json:"unable_to_verify"`
	NotCovered     int `json:"not_covered"`
}

// Finding is one discovered resource and its status.
type Finding struct {
	ResourceType     string            `json:"resource_type"` // aws_db_instance
	Status           string            `json:"status"`        // tracked, no_match, unable_to_verify, not_covered
	NotCoveredReason string            `json:"not_covered_reason,omitempty"`
	Partition        string            `json:"partition"`
	AccountID        string            `json:"account_id"`
	Region           string            `json:"region"`
	ARN              string            `json:"arn"`
	Identifier       string            `json:"identifier"`
	ResourceID       string            `json:"resource_id"`
	ImportID         string            `json:"import_id"` // what "terraform import" expects
	Engine           string            `json:"engine"`
	EngineVersion    string            `json:"engine_version"`
	Class            string            `json:"class"`
	InstanceStatus   string            `json:"instance_status"`
	Created          *time.Time        `json:"created,omitempty"`
	Tags             map[string]string `json:"tags"`
	Matches          []Match           `json:"matches"`
	Warnings         []Warning         `json:"warnings"`
}

// Match links a finding to one state entry.
type Match struct {
	Source     string `json:"source"`
	Workspace  string `json:"workspace"`
	Address    string `json:"address"`
	Lifecycle  string `json:"lifecycle"`
	DeposedKey string `json:"deposed_key,omitempty"`
	MatchedOn  string `json:"matched_on"`
	Weak       bool   `json:"weak"`
	Renamed    bool   `json:"renamed"`
}

// Warning names the sources and addresses involved, never state values.
type Warning struct {
	Kind      string   `json:"kind"`
	Sources   []string `json:"sources,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

// CostBlock is filled in by milestone M4.
type CostBlock struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// WriteJSON writes the report as indented JSON.
func WriteJSON(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}
