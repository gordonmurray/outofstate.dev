// Package match decides, for each discovered RDS instance, whether any
// supplied Terraform state tracks it. It is a pure function: no I/O.
package match

import (
	"slices"
	"strconv"
	"strings"

	"github.com/gordonmurray/outofstate.dev/internal/discover"
	"github.com/gordonmurray/outofstate.dev/internal/tfstate"
)

// SourceStatus is how completely a Terraform source was read.
type SourceStatus string

// Source statuses.
const (
	SourceOK      SourceStatus = "ok"
	SourcePartial SourceStatus = "partial"
	SourceFailed  SourceStatus = "failed"
)

// Source is one supplied Terraform state: a working directory or a file.
type Source struct {
	Name      string // path shown in the report
	Workspace string // "unknown" for state files
	Status    SourceStatus
	State     *tfstate.State // nil when Status is SourceFailed
}

// Status of one instance.
type Status string

// Instance statuses (PLAN.md §4).
const (
	StatusTracked    Status = "tracked"
	StatusNoMatch    Status = "no_match"
	StatusUnverified Status = "unable_to_verify"
	StatusNotCovered Status = "not_covered"
)

// Key is the identity key a match was made on.
type Key string

// Match keys, strongest first.
const (
	KeyResourceID Key = "resource_id"
	KeyARN        Key = "arn"        // weaker: state entry has no resource ID
	KeyIdentifier Key = "identifier" // weaker: state entry has no resource ID
)

// Match links an instance to one state entry.
type Match struct {
	Source    string
	Workspace string
	Identity  tfstate.Identity
	MatchedOn Key
	// Renamed is set when the resource IDs match but the ARNs differ: the
	// instance was renamed after the state was last written.
	Renamed bool
}

// Weak reports whether the match did not use the immutable resource ID.
func (m Match) Weak() bool { return m.MatchedOn != KeyResourceID }

// WarningKind classifies a warning.
type WarningKind string

// Warning kinds.
const (
	// Per instance.
	WarnMultipleOwners    WarningKind = "multiple_owners"    // matched in states with different lineage
	WarnMultipleAddresses WarningKind = "multiple_addresses" // matched at more than one address in one state
	WarnStaleStateEntry   WarningKind = "stale_state_entry"  // same ARN, different resource ID
	// Per scan.
	WarnDuplicateSnapshot WarningKind = "duplicate_snapshot" // same lineage and serial supplied twice
	WarnDivergedCopies    WarningKind = "diverged_copies"    // same lineage, different serial
)

// Warning carries the sources and addresses involved, never state values.
type Warning struct {
	Kind      WarningKind
	Sources   []string
	Addresses []string
}

// Finding is the result for one instance.
type Finding struct {
	Instance discover.Instance
	Status   Status
	Matches  []Match
	Warnings []Warning
}

// Result of matching.
type Result struct {
	Findings []Finding
	Warnings []Warning
	// StateComplete is true when every source loaded and parsed fully.
	StateComplete bool
}

// keptSource is a source whose identities take part in matching, after
// duplicate snapshots are removed.
type keptSource struct {
	Source
	owner string // lineage, or a per-source key when lineage is empty
}

// Run matches instances against sources.
func Run(instances []discover.Instance, sources []Source) Result {
	res := Result{StateComplete: true}
	for _, s := range sources {
		if s.Status != SourceOK || s.State == nil {
			res.StateComplete = false
		}
	}

	kept, warnings := dedupe(sources)
	res.Warnings = warnings

	for _, inst := range instances {
		res.Findings = append(res.Findings, matchInstance(inst, kept, res.StateComplete))
	}
	return res
}

// dedupe drops the second and later copies of an identical snapshot (same
// non-empty lineage and serial) and reports same-lineage snapshots with
// different serials. States without a lineage are never merged.
func dedupe(sources []Source) ([]keptSource, []Warning) {
	type snap struct {
		lineage string
		serial  int64
	}
	var (
		kept       []keptSource
		warnings   []Warning
		firstSnap  = map[snap]string{}
		bySerial   = map[string]map[int64][]string{} // lineage -> serial -> sources
		lineageOrd []string
	)
	for i, s := range sources {
		if s.State == nil {
			continue
		}
		owner := s.State.Lineage
		if owner == "" {
			kept = append(kept, keptSource{Source: s, owner: "\x00source-" + strconv.Itoa(i)})
			continue
		}
		key := snap{owner, s.State.Serial}
		if first, dup := firstSnap[key]; dup {
			warnings = append(warnings, Warning{Kind: WarnDuplicateSnapshot, Sources: []string{first, s.Name}})
			continue
		}
		firstSnap[key] = s.Name
		if bySerial[owner] == nil {
			bySerial[owner] = map[int64][]string{}
			lineageOrd = append(lineageOrd, owner)
		}
		bySerial[owner][s.State.Serial] = append(bySerial[owner][s.State.Serial], s.Name)
		kept = append(kept, keptSource{Source: s, owner: owner})
	}
	for _, lin := range lineageOrd {
		if len(bySerial[lin]) < 2 {
			continue
		}
		var names []string
		for _, s := range kept {
			if s.owner == lin {
				names = append(names, s.Name)
			}
		}
		warnings = append(warnings, Warning{Kind: WarnDivergedCopies, Sources: names})
	}
	return kept, warnings
}

func matchInstance(inst discover.Instance, sources []keptSource, stateComplete bool) Finding {
	f := Finding{Instance: inst}
	if inst.NotCovered != "" {
		f.Status = StatusNotCovered
		return f
	}

	owners := map[string]bool{}
	var ownerSources []string
	addrsBySource := make([][]string, len(sources))
	var staleSources, staleAddrs []string

	for si, src := range sources {
		for _, id := range src.State.Identities {
			if id.Partition != inst.Partition || id.AccountID != inst.AccountID || id.Region != inst.Region {
				continue
			}
			key, renamed, stale := compare(inst, id)
			if stale {
				staleSources = append(staleSources, src.Name)
				staleAddrs = append(staleAddrs, id.Address)
			}
			if key == "" {
				continue
			}
			f.Matches = append(f.Matches, Match{
				Source: src.Name, Workspace: src.Workspace, Identity: id, MatchedOn: key, Renamed: renamed,
			})
			if !owners[src.owner] {
				owners[src.owner] = true
				ownerSources = append(ownerSources, src.Name)
			}
			if !slices.Contains(addrsBySource[si], id.Address) {
				addrsBySource[si] = append(addrsBySource[si], id.Address)
			}
		}
	}

	switch {
	case len(f.Matches) > 0:
		f.Status = StatusTracked
	case stateComplete:
		f.Status = StatusNoMatch
	default:
		f.Status = StatusUnverified
	}

	if len(owners) > 1 {
		f.Warnings = append(f.Warnings, Warning{Kind: WarnMultipleOwners, Sources: ownerSources})
	}
	for si, src := range sources {
		if addrs := addrsBySource[si]; len(addrs) > 1 {
			f.Warnings = append(f.Warnings, Warning{Kind: WarnMultipleAddresses, Sources: []string{src.Name}, Addresses: addrs})
		}
	}
	if len(staleSources) > 0 {
		f.Warnings = append(f.Warnings, Warning{Kind: WarnStaleStateEntry, Sources: staleSources, Addresses: staleAddrs})
	}
	return f
}

// compare applies PLAN.md §6. When both sides have a resource ID, only the
// resource ID decides; a different resource ID with the same ARN is a stale
// state entry (the database was recreated) and never falls back to ARN or
// name. Fallback is allowed only when the state entry has no resource ID:
// an exact ARN, then the identifier within the same scope, is a weaker match.
func compare(inst discover.Instance, id tfstate.Identity) (key Key, renamed, stale bool) {
	if id.ResourceID != "" {
		// The state recorded the immutable ID, so only that ID can confirm
		// ownership. If AWS did not return one, the match cannot be verified.
		if inst.ResourceID == "" {
			return "", false, false
		}
		if inst.ResourceID == id.ResourceID {
			return KeyResourceID, inst.ARN != id.ARN, false
		}
		return "", false, inst.ARN != "" && inst.ARN == id.ARN
	}
	if inst.ARN != "" && inst.ARN == id.ARN {
		return KeyARN, false, false
	}
	if inst.Identifier != "" && strings.EqualFold(inst.Identifier, id.Identifier) {
		return KeyIdentifier, false, false
	}
	return "", false, false
}
