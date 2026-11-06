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
	Error     string // fixed category, never raw API output
	Instances []Instance
}

// Instances returns every instance from every successful region.
func (inv *Inventory) Instances() []Instance {
	var out []Instance
	for _, r := range inv.Regions {
		out = append(out, r.Instances...)
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
		rr := RegionResult{Region: r.Region, Failed: r.Error != "", Error: r.Error}
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
