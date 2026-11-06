package tfstate

import (
	"regexp"
	"strings"
)

// ARN is a parsed RDS DB-instance ARN:
// arn:<partition>:rds:<region>:<account>:db:<identifier>
type ARN struct {
	Partition  string
	Region     string
	AccountID  string
	Identifier string
}

var (
	partitionRE = regexp.MustCompile(`^aws(-[a-z]+)*$`)
	regionRE    = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
	accountRE   = regexp.MustCompile(`^[0-9]{12}$`)
	// RDS DB instance identifier: 1-63 letters, digits or hyphens, starting
	// with a letter, no consecutive hyphens, not ending with a hyphen.
	identifierRE = regexp.MustCompile(`^[A-Za-z](-?[A-Za-z0-9])*$`)
)

// ParseARN accepts only well-formed RDS DB-instance ARNs. Any other ARN (a
// cluster, a snapshot, another service, extra segments, whitespace or control
// characters) is rejected.
func ParseARN(s string) (ARN, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 7 || parts[0] != "arn" || parts[2] != "rds" || parts[5] != "db" {
		return ARN{}, false
	}
	a := ARN{Partition: parts[1], Region: parts[3], AccountID: parts[4], Identifier: parts[6]}
	if !partitionRE.MatchString(a.Partition) ||
		!regionRE.MatchString(a.Region) ||
		!accountRE.MatchString(a.AccountID) ||
		!ValidIdentifier(a.Identifier) {
		return ARN{}, false
	}
	return a, true
}

// ValidIdentifier reports whether s is a syntactically valid RDS DB instance
// identifier.
func ValidIdentifier(s string) bool {
	return len(s) <= 63 && identifierRE.MatchString(s)
}

// LBARN is a parsed Elastic Load Balancing v2 load balancer ARN:
// arn:<partition>:elasticloadbalancing:<region>:<account>:loadbalancer/<app|net|gwy>/<name>/<id>
type LBARN struct {
	Partition string
	Region    string
	AccountID string
	Kind      string // app, net, gwy
	Name      string
	ID        string
}

var (
	// Load balancer names: letters, digits and hyphens, not starting or
	// ending with a hyphen. Consecutive hyphens inside are allowed.
	lbNameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)
	lbIDRE   = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ParseLBARN accepts only well-formed ELBv2 load balancer ARNs. Target
// groups, listeners and Classic Load Balancers are rejected.
func ParseLBARN(s string) (LBARN, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "elasticloadbalancing" {
		return LBARN{}, false
	}
	res := strings.Split(parts[5], "/")
	if len(res) != 4 || res[0] != "loadbalancer" {
		return LBARN{}, false
	}
	a := LBARN{Partition: parts[1], Region: parts[3], AccountID: parts[4], Kind: res[1], Name: res[2], ID: res[3]}
	if !partitionRE.MatchString(a.Partition) || !regionRE.MatchString(a.Region) || !accountRE.MatchString(a.AccountID) {
		return LBARN{}, false
	}
	if a.Kind != "app" && a.Kind != "net" && a.Kind != "gwy" {
		return LBARN{}, false
	}
	if len(a.Name) > 32 || !lbNameRE.MatchString(a.Name) || !lbIDRE.MatchString(a.ID) {
		return LBARN{}, false
	}
	return a, true
}
