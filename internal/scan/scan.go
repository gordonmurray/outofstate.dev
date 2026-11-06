// Package scan ties discovery, Terraform state loading and matching together
// and builds the report. It does no rendering.
package scan

import (
	"errors"
	"os"
	"sort"
	"time"

	"github.com/gordonmurray/outofstate.dev/internal/discover"
	"github.com/gordonmurray/outofstate.dev/internal/match"
	"github.com/gordonmurray/outofstate.dev/internal/report"
	"github.com/gordonmurray/outofstate.dev/internal/tfstate"
)

// Exit codes (PLAN.md §3), evaluated in this order: 1, 3, 2, 0.
const (
	ExitOK      = 0
	ExitFatal   = 1
	ExitNoMatch = 2
	ExitPartial = 3
)

// Source error categories for state files. Messages are fixed text.
const (
	CategoryFileUnreadable = "file_unreadable"
)

var sourceMessages = map[string]string{
	CategoryFileUnreadable:               "could not read the state file",
	string(tfstate.CategoryNoSnapshot):   "the state is empty: no state snapshot",
	string(tfstate.CategoryInvalidJSON):  "not valid Terraform state JSON",
	string(tfstate.CategoryStateVersion): "not Terraform state format version 4",
}

// LoadedSource is a source plus what the report needs to say about it.
type LoadedSource struct {
	match.Source
	Kind          string // "file" or "directory"
	ErrorCategory string
	ObservedAt    time.Time
}

// LoadStateFile reads and parses one --state-file. Failures never stop the
// scan: they produce a failed source.
func LoadStateFile(path string, now func() time.Time) LoadedSource {
	ls := LoadedSource{
		Source: match.Source{Name: path, Workspace: "unknown"},
		Kind:   "file",
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is a user-supplied CLI argument
	ls.ObservedAt = now()
	if err != nil {
		ls.Status, ls.ErrorCategory = match.SourceFailed, CategoryFileUnreadable
		return ls
	}
	st, err := tfstate.Parse(data)
	if err != nil {
		var te *tfstate.Error
		ls.Status, ls.ErrorCategory = match.SourceFailed, string(tfstate.CategoryInvalidJSON)
		if errors.As(err, &te) {
			ls.ErrorCategory = string(te.Category)
		}
		return ls
	}
	ls.State = st
	ls.Status = match.SourceOK
	if st.Partial() {
		ls.Status = match.SourcePartial
	}
	return ls
}

// Input is everything a scan needs.
type Input struct {
	Inventory *discover.Inventory
	Sources   []LoadedSource
	Version   string
	Time      time.Time
}

// Run matches and builds the report.
func Run(in Input) *report.Report {
	srcs := make([]match.Source, len(in.Sources))
	for i, s := range in.Sources {
		srcs[i] = s.Source
	}
	res := match.Run(in.Inventory.Instances(), srcs)

	discoveryComplete := in.Inventory.Complete()
	r := &report.Report{
		SchemaVersion: report.SchemaVersion,
		Scan: report.Scan{
			Time:              in.Time.UTC(),
			Version:           in.Version,
			Partition:         in.Inventory.Partition,
			AccountID:         in.Inventory.AccountID,
			Complete:          res.StateComplete && discoveryComplete,
			StateComplete:     res.StateComplete,
			DiscoveryComplete: discoveryComplete,
		},
		Regions:  []report.Region{},
		Sources:  []report.Source{},
		Findings: []report.Finding{},
		Warnings: warnings(res.Warnings),
	}
	for _, reg := range in.Inventory.Regions {
		r.Scan.Regions = append(r.Scan.Regions, reg.Region)
		rr := report.Region{Region: reg.Region, Status: "ok", InstanceCount: len(reg.Instances)}
		if reg.Failed {
			rr.Status, rr.Error = "failed", reg.Error
		}
		r.Regions = append(r.Regions, rr)
	}
	for _, s := range in.Sources {
		r.Sources = append(r.Sources, source(s))
	}

	for _, f := range res.Findings {
		rf := finding(f)
		switch f.Status {
		case match.StatusTracked:
			r.Summary.Tracked++
		case match.StatusNoMatch:
			r.Summary.NoMatch++
		case match.StatusUnverified:
			r.Summary.UnableToVerify++
		case match.StatusNotCovered:
			r.Summary.NotCovered++
		}
		r.Findings = append(r.Findings, rf)
	}
	sortFindings(r.Findings)
	return r
}

// ExitCode applies PLAN.md §3 to a completed report. Code 1 (fatal) is
// decided by the caller when no report could be produced or written.
func ExitCode(r *report.Report) int {
	switch {
	case !r.Scan.Complete:
		return ExitPartial
	case r.Summary.NoMatch > 0:
		return ExitNoMatch
	default:
		return ExitOK
	}
}

func source(s LoadedSource) report.Source {
	rs := report.Source{
		Name: s.Name, Kind: s.Kind, Workspace: s.Workspace, Status: string(s.Status),
		ErrorCategory: s.ErrorCategory, Error: sourceMessages[s.ErrorCategory],
		ObservedAt: s.ObservedAt.UTC(),
	}
	if s.State != nil {
		rs.Lineage, rs.Serial, rs.IdentityCount = s.State.Lineage, s.State.Serial, len(s.State.Identities)
		for _, p := range s.State.Problems {
			rs.Problems = append(rs.Problems, report.Problem{Address: p.Address, Kind: string(p.Kind)})
		}
	}
	return rs
}

func finding(f match.Finding) report.Finding {
	in := f.Instance
	rf := report.Finding{
		ResourceType: "aws_db_instance", Status: string(f.Status), NotCoveredReason: in.NotCovered,
		Partition: in.Partition, AccountID: in.AccountID, Region: in.Region,
		ARN: in.ARN, Identifier: in.Identifier, ResourceID: in.ResourceID,
		ImportID: in.Identifier, // aws_db_instance imports by DB identifier
		Engine:   in.Engine, EngineVersion: in.EngineVersion, Class: in.Class, InstanceStatus: in.Status,
		Tags: in.Tags, Matches: []report.Match{}, Warnings: warnings(f.Warnings),
	}
	if rf.Tags == nil {
		rf.Tags = map[string]string{}
	}
	if !in.Created.IsZero() {
		c := in.Created.UTC()
		rf.Created = &c
	}
	for _, m := range f.Matches {
		rf.Matches = append(rf.Matches, report.Match{
			Source: m.Source, Workspace: m.Workspace, Address: m.Identity.Address,
			Lifecycle: string(m.Identity.Lifecycle), DeposedKey: m.Identity.DeposedKey,
			MatchedOn: string(m.MatchedOn), Weak: m.Weak(), Renamed: m.Renamed,
		})
	}
	return rf
}

func warnings(ws []match.Warning) []report.Warning {
	out := []report.Warning{}
	for _, w := range ws {
		out = append(out, report.Warning{Kind: string(w.Kind), Sources: w.Sources, Addresses: w.Addresses})
	}
	return out
}

var statusRank = map[string]int{"no_match": 0, "unable_to_verify": 1, "tracked": 2, "not_covered": 3}

// sortFindings puts import candidates first, then a stable order. Cost-based
// ordering arrives with M4.
func sortFindings(fs []report.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if statusRank[a.Status] != statusRank[b.Status] {
			return statusRank[a.Status] < statusRank[b.Status]
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		return a.Identifier < b.Identifier
	})
}
