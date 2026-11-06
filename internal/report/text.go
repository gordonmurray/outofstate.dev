package report

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// WriteText writes the human-readable report. The same content is written
// to a terminal or a file; only colour (added in M3) differs.
func WriteText(w io.Writer, r *Report) error {
	p := &printer{w: w}

	scope := strings.Join(r.Scan.Regions, ", ")
	p.f("Out of State · account %s · %s · %s\n", clean(r.Scan.AccountID), clean(scope), r.Scan.Time.UTC().Format("2006-01-02 15:04 MST"))
	if r.Scan.Complete {
		p.f("Scan: complete\n")
	} else {
		p.f("Scan: PARTIAL. Results below may be incomplete.\n")
	}
	for _, s := range r.Sources {
		mark := "✓"
		switch s.Status {
		case "partial":
			mark = "partial"
		case "failed":
			mark = "FAILED"
		}
		p.f("  source  %s (%s) %s\n", clean(s.Name), clean(s.Workspace), mark)
		if s.Error != "" {
			p.f("          %s\n", s.Error)
		}
		if s.Status == "partial" {
			p.f("          %d aws_db_instance object(s) in this state could not be read\n", len(s.Problems))
		}
	}
	for _, reg := range r.Regions {
		if reg.Status == "failed" {
			p.f("  region  %s FAILED: %s\n", clean(reg.Region), reg.Error)
		}
	}
	p.f("\n")

	inScope := r.Summary.Tracked + r.Summary.NoMatch + r.Summary.UnableToVerify
	if inScope == 0 && r.Summary.NotCovered == 0 {
		p.f("0 in-scope RDS instances in %s\n", clean(scope))
	}

	candidates := filter(r.Findings, "no_match")
	unverified := filter(r.Findings, "unable_to_verify")
	tracked := filter(r.Findings, "tracked")
	notCovered := filter(r.Findings, "not_covered")

	if len(candidates) > 0 {
		p.f("%d import candidate(s): RDS instances with no match in the supplied states\n\n", len(candidates))
		for _, f := range candidates {
			p.f("  %-24s %-16s %-16s %s\n", clean(f.Identifier), clean(f.Class), clean(f.Engine+" "+f.EngineVersion), clean(f.InstanceStatus))
		}
		p.f("\n")
	}
	if len(unverified) > 0 {
		p.f("%d unable to verify: no match, but a source could not be read fully\n\n", len(unverified))
		for _, f := range unverified {
			p.f("  %-24s %-16s %s\n", clean(f.Identifier), clean(f.Class), clean(f.Engine+" "+f.EngineVersion))
		}
		p.f("\n")
	}
	if len(tracked) > 0 {
		p.f("%d tracked in state\n", len(tracked))
		for _, f := range tracked {
			m := f.Matches[0]
			extra := ""
			if len(f.Matches) > 1 {
				extra = fmt.Sprintf(" (+%d more)", len(f.Matches)-1)
			}
			if m.Weak {
				extra += " (matched on " + m.MatchedOn + ")"
			}
			p.f("  %-24s %-16s %s%s\n", clean(f.Identifier), clean(m.Source), clean(m.Address), extra)
		}
		p.f("\n")
	}
	if len(notCovered) > 0 {
		p.f("%d not covered\n", len(notCovered))
		for _, f := range notCovered {
			p.f("  %-24s %s\n", clean(f.Identifier), f.NotCoveredReason)
		}
		p.f("\n")
	}

	warnings := append([]Warning{}, r.Warnings...)
	type fw struct {
		id string
		w  Warning
	}
	var findingWarnings []fw
	for _, f := range r.Findings {
		for _, w := range f.Warnings {
			findingWarnings = append(findingWarnings, fw{f.Identifier, w})
		}
	}
	if len(warnings)+len(findingWarnings) > 0 {
		p.f("Warnings\n")
		for _, w := range warnings {
			p.f("  %s\n", describe(w))
		}
		for _, x := range findingWarnings {
			p.f("  %s: %s\n", clean(x.id), describe(x.w))
		}
		p.f("\n")
	}

	evidence := append(append([]Finding{}, candidates...), unverified...)
	if len(evidence) > 0 {
		var checked []string
		for _, s := range r.Sources {
			checked = append(checked, clean(s.Name)+" ("+clean(s.Workspace)+", "+s.Status+")")
		}
		p.f("Evidence\n")
		for _, f := range evidence {
			p.f("  %s\n", clean(f.Identifier))
			p.f("    arn          %s\n", clean(f.ARN))
			p.f("    resource id  %s\n", clean(f.ResourceID))
			p.f("    import id    %s\n", clean(f.ImportID))
			p.f("    region       %s\n", clean(f.Region))
			p.f("    engine       %s · %s · status %s\n", clean(f.Engine+" "+f.EngineVersion), clean(f.Class), clean(f.InstanceStatus))
			if len(f.Tags) > 0 {
				p.f("    tags         %s\n", tags(f.Tags))
			}
			p.f("    checked      %s\n", strings.Join(checked, ", "))
		}
		p.f("\n")
		p.f("Import candidates need a configuration review and a terraform plan before applying.\n")
	}
	return p.err
}

type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, a ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, a...)
}

func filter(fs []Finding, status string) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Status == status {
			out = append(out, f)
		}
	}
	return out
}

func describe(w Warning) string {
	srcs := cleanAll(w.Sources)
	addrs := cleanAll(w.Addresses)
	switch w.Kind {
	case "multiple_owners":
		return "managed by more than one state: " + strings.Join(srcs, ", ")
	case "multiple_addresses":
		return "in " + strings.Join(srcs, ", ") + " at more than one address: " + strings.Join(addrs, ", ")
	case "stale_state_entry":
		return "stale state entry (same ARN, different resource ID; the database was probably recreated) at " +
			strings.Join(addrs, ", ") + " in " + strings.Join(srcs, ", ")
	case "duplicate_snapshot":
		return "the same state snapshot was supplied more than once: " + strings.Join(srcs, ", ") + " (counted once)"
	case "diverged_copies":
		return "different versions of the same state were supplied: " + strings.Join(srcs, ", ")
	default:
		return w.Kind
	}
}

func tags(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, clean(k)+"="+clean(m[k]))
	}
	return strings.Join(parts, ", ")
}

func cleanAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = clean(s)
	}
	return out
}

// clean makes a value from AWS or state safe to print on a terminal:
// non-printable characters (including escape sequences) are shown escaped.
func clean(s string) string {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			q := strconv.QuoteToGraphic(s)
			return q[1 : len(q)-1]
		}
	}
	return s
}
