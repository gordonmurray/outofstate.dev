package report

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// TextOptions controls text rendering.
type TextOptions struct {
	// Color adds ANSI colours. Use it only for a terminal; the words are the
	// same either way.
	Color bool
}

// WriteText writes the human-readable report without colour.
func WriteText(w io.Writer, r *Report) error { return WriteTextWith(w, r, TextOptions{}) }

// WriteTextWith writes the human-readable report. The content is the same
// for a terminal or a file; only colour differs.
func WriteTextWith(w io.Writer, r *Report, o TextOptions) error {
	p := &printer{w: w, color: o.Color}

	scope := strings.Join(r.Scan.Regions, ", ")
	p.f("Out of State · account %s · %s · %s\n", clean(r.Scan.AccountID), clean(scope), r.Scan.Time.UTC().Format("2006-01-02 15:04 MST"))
	if r.Scan.Complete {
		p.f("Scan: complete\n")
	} else {
		p.f("%s\n", p.c(yellow, "Scan: PARTIAL. Results below may be incomplete."))
	}
	for _, s := range r.Sources {
		mark := p.c(green, "✓")
		switch s.Status {
		case "partial":
			mark = p.c(yellow, "partial")
		case "failed":
			mark = p.c(red, "FAILED")
		}
		p.f("  source  %s (%s) %s %s\n", clean(s.Name), clean(s.Workspace), mark, s.ObservedAt.UTC().Format("15:04:05"))
		if s.Error != "" {
			p.f("          %s\n", clean(s.Error))
		}
		if s.Hint != "" {
			p.f("          to see Terraform's output, run: %s\n", clean(s.Hint))
		}
		if s.Status == "partial" {
			p.f("          %d aws_db_instance object(s) in this state could not be read\n", len(s.Problems))
		}
	}
	for _, reg := range r.Regions {
		if reg.Status == "failed" {
			p.f("  region  %s %s: %s\n", clean(reg.Region), p.c(red, "FAILED"), clean(reg.Error))
		}
	}
	p.f("\n")

	inScope := r.Summary.Tracked + r.Summary.NoMatch + r.Summary.UnableToVerify
	if inScope == 0 {
		p.f("0 in-scope RDS instances in %s\n\n", clean(scope))
	}

	candidates := filter(r.Findings, "no_match")
	unverified := filter(r.Findings, "unable_to_verify")
	tracked := filter(r.Findings, "tracked")
	notCovered := filter(r.Findings, "not_covered")

	if len(candidates) > 0 {
		p.f("%s\n\n", p.c(bold, fmt.Sprintf("%d import candidate(s): RDS instances with no match in the supplied states", len(candidates))))
		for _, f := range candidates {
			p.f("  %s %-16s %-16s %-12s %s\n", p.c(grey, padName(clean(f.Identifier))), clean(f.Class), clean(f.Engine+" "+f.EngineVersion), clean(f.InstanceStatus), created(f))
		}
		p.f("\n")
	}
	if len(unverified) > 0 {
		p.f("%s\n\n", p.c(yellow, fmt.Sprintf("%d unable to verify: no match, but a source could not be read fully", len(unverified))))
		for _, f := range unverified {
			p.f("  %s %-16s %-16s %-12s %s\n", p.c(yellow, padName(clean(f.Identifier))), clean(f.Class), clean(f.Engine+" "+f.EngineVersion), clean(f.InstanceStatus), created(f))
		}
		p.f("\n")
	}
	if len(tracked) > 0 {
		p.f("%s\n", p.c(green, fmt.Sprintf("%d tracked in state", len(tracked))))
		for _, f := range tracked {
			for i, m := range f.Matches {
				name := padName(clean(f.Identifier))
				if i > 0 {
					name = padName("")
				}
				extra := " (" + m.MatchedOn
				if m.Weak {
					extra += ", weaker match"
				}
				if m.Renamed {
					extra += ", renamed since state was written"
				}
				if m.Lifecycle != "current" {
					extra += ", " + m.Lifecycle
				}
				extra += ")"
				p.f("  %s %s (%s) %s%s\n", p.c(green, name), clean(m.Source), clean(m.Workspace), clean(m.Address), extra)
			}
		}
		p.f("\n")
	}
	if len(notCovered) > 0 {
		p.f("%s\n", p.c(dim, fmt.Sprintf("%d not covered", len(notCovered))))
		for _, f := range notCovered {
			p.f("  %s %s\n", p.c(dim, padName(clean(f.Identifier))), p.c(dim, f.NotCoveredReason))
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
		p.f("%s\n", p.c(yellow, "Warnings"))
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
			if f.Created != nil {
				p.f("    created      %s\n", f.Created.UTC().Format("2006-01-02"))
			}
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
	w     io.Writer
	err   error
	color bool
}

// ANSI styles. Applied only to text oos wrote or already escaped with clean.
const (
	bold   = "1"
	dim    = "2"
	red    = "31"
	green  = "32"
	yellow = "33"
	grey   = "90"
)

func (p *printer) c(style, s string) string {
	if !p.color || s == "" {
		return s
	}
	return "\x1b[" + style + "m" + s + "\x1b[0m"
}

// nameWidth is the width of the first column.
const nameWidth = 24

// padName right-pads s to nameWidth runes (fmt's %-24s would count escape
// codes added by colour).
func padName(s string) string {
	width := nameWidth
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

func (p *printer) f(format string, a ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, a...)
}

func created(f Finding) string {
	if f.Created == nil {
		return ""
	}
	return "created " + f.Created.UTC().Format("2006-01-02")
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
	case "tags_unavailable":
		return "tags could not be read (rds:ListTagsForResource)"
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
