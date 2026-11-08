// Package scan ties discovery, Terraform state loading and matching together
// and builds the report. It does no rendering.
package scan

import (
	"context"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/gordonmurray/outofstate.dev/internal/cost"
	"github.com/gordonmurray/outofstate.dev/internal/discover"
	"github.com/gordonmurray/outofstate.dev/internal/match"
	"github.com/gordonmurray/outofstate.dev/internal/report"
	"github.com/gordonmurray/outofstate.dev/internal/tfrun"
	"github.com/gordonmurray/outofstate.dev/internal/tfstate"
)

// Exit codes, evaluated in this order: 1, 3, 2, 0.
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
	ErrorMessage  string // fixed text; empty means the default for the category
	Hint          string // command to run by hand to see Terraform's own output
	ObservedAt    time.Time
}

// LoadDirectory runs "state pull" in a Terraform working directory and
// parses the result. Failures never stop the scan: they produce a failed
// source with a fixed message and a hint.
func LoadDirectory(ctx context.Context, dir string, opts tfrun.Options, now func() time.Time) LoadedSource {
	ws, data, err := tfrun.Pull(ctx, dir, opts)
	ls := LoadedSource{
		Source:     match.Source{Name: dir, Workspace: ws},
		Kind:       "directory",
		ObservedAt: now(),
	}
	if ls.Workspace == "" {
		ls.Workspace = "unknown"
	}
	if err != nil {
		ls.Status, ls.ErrorCategory = match.SourceFailed, string(err.Category)
		ls.ErrorMessage, ls.Hint = err.Message(), err.Hint()
		return ls
	}
	st, perr := tfstate.Parse(data)
	if perr != nil {
		ls.Status, ls.ErrorCategory = match.SourceFailed, string(tfstate.CategoryInvalidJSON)
		var te *tfstate.Error
		if errors.As(perr, &te) {
			ls.ErrorCategory = string(te.Category)
		}
		hint := &tfrun.Error{Category: tfrun.CategoryExitError, Binary: opts.Binary, Dir: dir, Workspace: ws}
		ls.Hint = hint.Hint()
		return ls
	}
	ls.State = st
	ls.Status = match.SourceOK
	if st.Partial() {
		ls.Status = match.SourcePartial
	}
	return ls
}

// LoadDirectories loads several directories, at most limit at a time,
// keeping the input order.
func LoadDirectories(ctx context.Context, dirs []string, opts tfrun.Options, limit int, now func() time.Time) []LoadedSource {
	out := make([]LoadedSource, len(dirs))
	slots := make(chan struct{}, max(1, limit))
	var wg sync.WaitGroup
	for i, d := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			out[i] = LoadDirectory(ctx, d, opts, now)
		}()
	}
	wg.Wait()
	return out
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
	Cost      *cost.Result // nil when cost was not requested
	// Types limits state to the Terraform types being checked, so problems
	// with other types in a state cannot make the scan partial. Nil means
	// every type.
	Types   []string
	Version string
	Time    time.Time
}

// CostScope says what the cost figures cover.
const CostScope = "all RDS charges in the scanned account and regions (instances, storage, backups and other usage), not the cost of particular findings"

func costBlock(c *cost.Result) *report.CostBlock {
	if c == nil {
		return nil
	}
	if !c.Available {
		return &report.CostBlock{Available: false, Reason: string(c.Reason), Message: c.Reason.Message()}
	}
	b := &report.CostBlock{Available: true, Scope: CostScope, Metric: cost.Metric, Currency: c.Currency}
	for _, p := range []struct {
		name string
		p    *cost.Period
	}{{"last_6_months", c.Months}, {"last_30_days", c.Last30}} {
		bd := map[string]string{}
		for _, bucket := range cost.Buckets {
			bd[string(bucket)] = cost.Decimal(p.p.Breakdown[bucket])
		}
		b.Periods = append(b.Periods, report.CostPeriod{
			Name: p.name, Label: p.p.Label, Start: p.p.Start, End: p.p.End,
			Total: cost.Decimal(p.p.Total), Estimated: p.p.Estimated, Breakdown: bd,
		})
	}
	return b
}

// Run matches and builds the report.
func Run(in Input) *report.Report {
	if in.Types != nil {
		for i := range in.Sources {
			restrictTypes(&in.Sources[i], in.Types)
		}
	}
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
			Regions:           []string{},
			Complete:          res.StateComplete && discoveryComplete,
			StateComplete:     res.StateComplete,
			DiscoveryComplete: discoveryComplete,
		},
		Regions:  []report.Region{},
		Sources:  []report.Source{},
		Findings: []report.Finding{},
		Warnings: warnings(res.Warnings),
		Cost:     costBlock(in.Cost),
	}
	r.Scan.Regions = append(r.Scan.Regions, in.Inventory.RegionNames()...)
	for _, reg := range in.Inventory.Regions {
		typ := reg.Type
		if typ == "" {
			typ = discover.TypeDBInstance
		}
		rr := report.Region{Region: reg.Region, ResourceType: typ, Status: "ok", InstanceCount: len(reg.Instances)}
		if reg.Failed {
			rr.Status, rr.ErrorCategory, rr.Error = "failed", string(reg.Error), reg.Error.Message()
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

// restrictTypes keeps only the selected types in a loaded source and
// recomputes whether it is partial.
func restrictTypes(s *LoadedSource, types []string) {
	if s.State == nil {
		return
	}
	st := *s.State
	st.Identities = append([]tfstate.Identity(nil), st.Identities...)
	st.Problems = append([]tfstate.Problem(nil), st.Problems...)
	st.Keep(types)
	s.State = &st
	if s.Status == match.SourcePartial && !st.Partial() {
		s.Status = match.SourceOK
	}
}

// ExitCode applies the exit-code rules to a completed report. Code 1 (fatal) is
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
		ErrorCategory: s.ErrorCategory, Error: s.ErrorMessage, Hint: s.Hint,
		ObservedAt: s.ObservedAt.UTC(),
	}
	if rs.Error == "" {
		rs.Error = sourceMessages[s.ErrorCategory]
	}
	if s.State != nil {
		lineage, serial := s.State.Lineage, s.State.Serial
		rs.Lineage, rs.Serial, rs.IdentityCount = &lineage, &serial, len(s.State.Identities)
		for _, p := range s.State.Problems {
			rs.Problems = append(rs.Problems, report.Problem{Address: p.Address, ResourceType: p.Type, Kind: string(p.Kind)})
		}
	}
	return rs
}

func finding(f match.Finding) report.Finding {
	in := f.Instance
	rf := report.Finding{
		ResourceType: in.TerraformType(), Kind: in.Kind, Status: string(f.Status), NotCoveredReason: in.NotCovered,
		Partition: in.Partition, AccountID: in.AccountID, Region: in.Region,
		ARN: in.ARN, Identifier: in.Identifier, ResourceID: in.ResourceID,
		ImportID: in.Identifier, // aws_db_instance imports by DB identifier; aws_lb by ARN (below)
		Engine:   in.Engine, EngineVersion: in.EngineVersion, Class: in.Class, InstanceStatus: in.Status,
		Tags: in.Tags, Matches: []report.Match{}, Warnings: warnings(f.Warnings),
	}
	if rf.Tags == nil {
		rf.Tags = map[string]string{}
	}
	if in.TerraformType() == discover.TypeLoadBalancer {
		rf.ImportID = in.ARN
		rf.Details = map[string]string{"scheme": in.Scheme, "vpc_id": in.VPCID}
	}
	if in.TerraformType() == discover.TypeLambdaFunction {
		rf.Details = map[string]string{
			"runtime": in.Runtime, "package_type": in.PackageType,
			"architecture": in.Architecture, "last_modified": in.LastModified,
		}
	}
	if in.TerraformType() == discover.TypeNATGateway {
		rf.Details = map[string]string{
			"name": in.Tags["Name"], "vpc_id": in.VPCID,
			"availability_mode": in.AvailabilityMode, "connectivity_type": in.ConnectivityType,
		}
		if in.SubnetID != "" { // regional gateways have no subnet
			rf.Details["subnet_id"] = in.SubnetID
		}
	}
	if rf.Kind == "" {
		rf.Kind = discover.KindRDS
	}
	if in.TagsUnavailable {
		rf.Warnings = append(rf.Warnings, report.Warning{Kind: "tags_unavailable"})
	}
	// A hint, not a verdict: the tags suggest another tool manages it.
	if f.Status == match.StatusNoMatch || f.Status == match.StatusUnverified {
		switch {
		case in.ManagedBy != "":
			rf.Warnings = append(rf.Warnings, report.Warning{Kind: "managed_by_other_tool", Tool: in.ManagedBy})
		case in.LikelyCreatedBy != "":
			rf.Warnings = append(rf.Warnings, report.Warning{Kind: "likely_created_by", Tool: in.LikelyCreatedBy})
		}
	}
	if !in.Created.IsZero() {
		c := in.Created.UTC()
		rf.Created = &c
	}
	for _, m := range f.Matches {
		rf.Matches = append(rf.Matches, report.Match{
			Source: m.Source, Workspace: m.Workspace, Address: m.Identity.Address,
			Lifecycle: string(m.Identity.Lifecycle), DeposedKey: m.Identity.DeposedKey,
			MatchedOn: string(m.MatchedOn), Weak: m.Weak, Renamed: m.Renamed,
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
