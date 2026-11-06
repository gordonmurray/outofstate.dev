// Package discover lists AWS resources. For the MVP: RDS DB instances.
package discover

import "time"

// Instance is one RDS DB instance as seen in AWS.
type Instance struct {
	Partition string
	AccountID string
	Region    string

	ARN        string
	Identifier string
	ResourceID string // DbiResourceId, "db-..."

	Engine        string
	EngineVersion string
	Class         string
	Status        string // DBInstanceStatus: available, stopped, modifying, ...
	ClusterID     string // DBClusterIdentifier, non-empty for cluster members
	Tags          map[string]string
	Created       time.Time

	// NotCovered is non-empty when the instance is outside MVP scope, and
	// says why. Set by Classify.
	NotCovered string
}

// Reasons an instance is not covered by the MVP.
const (
	NotCoveredClusterMember = "member of an Aurora or Multi-AZ DB cluster"
	NotCoveredDocumentDB    = "DocumentDB instance"
	NotCoveredNeptune       = "Neptune instance"
)

// Classify sets NotCovered. Ordinary Multi-AZ DB instances stay in scope.
func Classify(inst *Instance) {
	switch {
	case inst.Engine == "docdb":
		inst.NotCovered = NotCoveredDocumentDB
	case inst.Engine == "neptune":
		inst.NotCovered = NotCoveredNeptune
	case inst.ClusterID != "":
		inst.NotCovered = NotCoveredClusterMember
	default:
		inst.NotCovered = ""
	}
}
