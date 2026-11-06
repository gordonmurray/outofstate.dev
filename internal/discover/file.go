package discover

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Inventory is the result of discovery for one account.
type Inventory struct {
	Partition string
	AccountID string
	Regions   []RegionResult
}

// RegionResult is the discovery result for one region. A failed region has
// no instances, and makes the scan partial.
type RegionResult struct {
	Region    string
	Failed    bool
	Error     ErrorCategory // fixed category, never raw API output
	Instances []Instance
}

// ErrorCategory is a fixed class of discovery failure. Raw AWS error text is
// never carried into the report: it can contain account details.
type ErrorCategory string

// Discovery error categories.
const (
	ErrAccessDenied   ErrorCategory = "access_denied"
	ErrThrottled      ErrorCategory = "throttled"
	ErrTimeout        ErrorCategory = "timeout"
	ErrRegionDisabled ErrorCategory = "region_disabled"
	ErrAPI            ErrorCategory = "api_error"
)

var errorMessages = map[ErrorCategory]string{
	ErrAccessDenied:   "access denied (check the IAM permissions in the README)",
	ErrThrottled:      "AWS throttled the requests; try again",
	ErrTimeout:        "timed out",
	ErrRegionDisabled: "the region is not enabled for this account",
	ErrAPI:            "the AWS API returned an error",
}

// Message returns the fixed, printable message for a category.
func (c ErrorCategory) Message() string {
	if m, ok := errorMessages[c]; ok {
		return m
	}
	return errorMessages[ErrAPI]
}

// knownCategory maps any value to a known category.
func knownCategory(s string) ErrorCategory {
	if _, ok := errorMessages[ErrorCategory(s)]; ok {
		return ErrorCategory(s)
	}
	return ErrAPI
}

// Instances returns every instance from every successful region.
func (inv *Inventory) Instances() []Instance {
	var out []Instance
	for _, r := range inv.Regions {
		out = append(out, r.Instances...)
	}
	return out
}

// RegionNames lists every scanned region, in order.
func (inv *Inventory) RegionNames() []string {
	out := make([]string, 0, len(inv.Regions))
	for _, r := range inv.Regions {
		out = append(out, r.Region)
	}
	return out
}

// Complete reports whether every region was scanned successfully.
func (inv *Inventory) Complete() bool {
	for _, r := range inv.Regions {
		if r.Failed {
			return false
		}
	}
	return true
}

// inventoryFile is the JSON shape of a fake inventory, used by tests and by
// the hidden --inventory-file flag until live discovery exists.
type inventoryFile struct {
	Partition string `json:"partition"`
	AccountID string `json:"account_id"`
	Regions   []struct {
		Region    string `json:"region"`
		Error     string `json:"error"`
		Instances []struct {
			ARN           string            `json:"arn"`
			Identifier    string            `json:"identifier"`
			ResourceID    string            `json:"resource_id"`
			Engine        string            `json:"engine"`
			EngineVersion string            `json:"engine_version"`
			Class         string            `json:"class"`
			Status        string            `json:"status"`
			ClusterID     string            `json:"cluster_id"`
			Tags          map[string]string `json:"tags"`
			Created       time.Time         `json:"created"`
		} `json:"instances"`
	} `json:"regions"`
}

// LoadFile reads a fake inventory from a JSON file.
func LoadFile(path string) (*Inventory, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is a user-supplied CLI argument
	if err != nil {
		return nil, fmt.Errorf("reading inventory file: %w", err)
	}
	var f inventoryFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("inventory file %s is not valid JSON", path)
	}
	if f.Partition == "" || f.AccountID == "" {
		return nil, fmt.Errorf("inventory file %s needs partition and account_id", path)
	}
	inv := &Inventory{Partition: f.Partition, AccountID: f.AccountID}
	for _, r := range f.Regions {
		rr := RegionResult{Region: r.Region, Failed: r.Error != ""}
		if rr.Failed {
			rr.Error = knownCategory(r.Error)
		}
		if !rr.Failed {
			for _, i := range r.Instances {
				inst := Instance{
					Partition: f.Partition, AccountID: f.AccountID, Region: r.Region,
					ARN: i.ARN, Identifier: i.Identifier, ResourceID: i.ResourceID,
					Engine: i.Engine, EngineVersion: i.EngineVersion, Class: i.Class,
					Status: i.Status, ClusterID: i.ClusterID, Tags: i.Tags, Created: i.Created,
				}
				Classify(&inst)
				rr.Instances = append(rr.Instances, inst)
			}
		}
		inv.Regions = append(inv.Regions, rr)
	}
	return inv, nil
}
