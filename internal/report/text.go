package report

import (
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"unicode"

	"github.com/gordonmurray/outofstate.dev/internal/cost"
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
			p.f("  region  %s (%s) %s: %s\n", clean(reg.Region), typeLabel(reg.ResourceType), p.c(red, "FAILED"), clean(reg.Error))
		}
	}
	p.costLines(r.Cost, scope)
	p.f("\n")

	inScope := r.Summary.Tracked + r.Summary.NoMatch + r.Summary.UnableToVerify
	if inScope == 0 {
		p.f("0 in-scope resources in %s\n\n", clean(scope))
	}

	candidates := filter(r.Findings, "no_match")
	unverified := filter(r.Findings, "unable_to_verify")
	tracked := filter(r.Findings, "tracked")
	notCovered := filter(r.Findings, "not_covered")

	if len(candidates) > 0 {
		p.f("%s\n\n", p.c(bold, fmt.Sprintf("%d import candidate(s): resources with no match in the supplied states", len(candidates))))
		for _, f := range candidates {
			c1, c2 := columns(f)
			p.f("  %s %s %s %-16s %-24s %-12s %s\n", p.c(grey, padName(clean(f.Identifier))), padRegion(clean(f.Region)), padKind(clean(f.Kind)), c1, c2, clean(f.InstanceStatus), created(f))
		}
		p.f("\n")
	}
	if len(unverified) > 0 {
		p.f("%s\n\n", p.c(yellow, fmt.Sprintf("%d unable to verify: no match, but a source could not be read fully", len(unverified))))
		for _, f := range unverified {
			c1, c2 := columns(f)
			p.f("  %s %s %s %-16s %-24s %-12s %s\n", p.c(yellow, padName(clean(f.Identifier))), padRegion(clean(f.Region)), padKind(clean(f.Kind)), c1, c2, clean(f.InstanceStatus), created(f))
		}
		p.f("\n")
	}
	if len(tracked) > 0 {
		p.f("%s\n", p.c(green, fmt.Sprintf("%d tracked in state", len(tracked))))
		for _, f := range tracked {
			for i, m := range f.Matches {
				name, region, kind := padName(clean(f.Identifier)), padRegion(clean(f.Region)), padKind(clean(f.Kind))
				if i > 0 {
					name, region, kind = padName(""), padRegion(""), padKind("")
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
				p.f("  %s %s %s %s (%s) %s%s\n", p.c(green, name), region, kind, clean(m.Source), clean(m.Workspace), clean(m.Address), extra)
			}
		}
		p.f("\n")
	}
	if len(notCovered) > 0 {
		p.f("%s\n", p.c(dim, fmt.Sprintf("%d not covered", len(notCovered))))
		for _, f := range notCovered {
			p.f("  %s %s %s %s\n", p.c(dim, padName(clean(f.Identifier))), padRegion(clean(f.Region)), padKind(clean(f.Kind)), p.c(dim, f.NotCoveredReason))
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

	if len(candidates)+len(unverified) > 0 {
		p.f("Import candidates need a configuration review and a terraform plan before applying.\n")
		p.f("%s\n", p.c(dim, "Use --format json for full details (ARNs, resource and import IDs, tags)."))
	}
	return p.err
}

// bucketNames are the display names for cost breakdown buckets, in order.
var bucketNames = []struct{ key, name string }{
	{"instances", "instances"}, {"multi_az", "Multi-AZ"}, {"storage", "storage"},
	{"backup", "backup"}, {"proxy", "proxy"}, {"other", "other"},
}

func (p *printer) costLines(c *CostBlock, scope string) {
	if c == nil {
		return
	}
	if !c.Available {
		p.f("RDS charges: unavailable (%s)\n", clean(c.Message))
		return
	}
	var parts []string
	var last30 *CostPeriod
	for i := range c.Periods {
		cp := &c.Periods[i]
		s := money(cp.Total, c.Currency) + " " + cp.Label
		if cp.Estimated {
			s += " (estimated)"
		}
		parts = append(parts, s)
		if cp.Name == "last_30_days" {
			last30 = cp
		}
	}
	p.f("RDS charges in %s (amortised, %s): %s\n", clean(scope), clean(c.Currency), strings.Join(parts, " · "))
	if last30 != nil {
		var items []string
		for _, b := range bucketNames {
			if v := last30.Breakdown[b.key]; v != "" && !isZero(v) {
				items = append(items, b.name+" "+money(v, c.Currency))
			}
		}
		if len(items) > 0 {
			p.f("  last 30 days: %s\n", strings.Join(items, " · "))
		}
	}
	p.f("  %s\n", p.c(dim, "All RDS charges in these regions, not the cost of the import candidates below."))
}

func money(decimal, currency string) string {
	r, ok := new(big.Rat).SetString(decimal)
	if !ok {
		return clean(decimal)
	}
	return cost.Money(r, clean(currency))
}

func isZero(decimal string) bool {
	r, ok := new(big.Rat).SetString(decimal)
	return ok && r.Sign() == 0
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
func padName(s string) string { return pad(s, nameWidth) }

// regionWidth fits every AWS region name (for example ap-southeast-3).
const regionWidth = 15

func padRegion(s string) string { return pad(s, regionWidth) }

// kindWidth fits the longest kind label (gwlb).
const kindWidth = 5

func padKind(s string) string { return pad(s, kindWidth) }

// columns returns the two type-specific columns for a resource row.
func columns(f Finding) (string, string) {
	switch f.ResourceType {
	case "aws_lb":
		return clean(f.Details["scheme"]), clean(f.Details["vpc_id"])
	default:
		return clean(f.Class), clean(f.Engine + " " + f.EngineVersion)
	}
}

func typeLabel(resourceType string) string {
	switch resourceType {
	case "aws_lb":
		return "load balancers"
	case "", "aws_db_instance":
		return "RDS DB instances"
	}
	return clean(resourceType)
}

func toolLabel(tool string) string {
	switch tool {
	case "cloudformation":
		return "CloudFormation"
	case "elastic_beanstalk":
		return "Elastic Beanstalk"
	case "kubernetes":
		return "Kubernetes (AWS Load Balancer Controller)"
	}
	return clean(tool)
}

func pad(s string, width int) string {
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
	case "managed_by_other_tool":
		return "its tags suggest it is managed by " + toolLabel(w.Tool) + ", not Terraform; check there before importing"
	case "tags_unavailable":
		return "tags could not be read (check the tag-reading permissions in the README)"
	case "diverged_copies":
		if len(srcs) > 1 {
			return "several versions of the same state were supplied; only the newest, " + srcs[0] +
				", was used (ignored: " + strings.Join(srcs[1:], ", ") + ")"
		}
		return "several versions of the same state were supplied: " + strings.Join(srcs, ", ")
	default:
		return w.Kind
	}
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
