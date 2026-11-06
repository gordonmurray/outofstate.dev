// Package cost reads RDS spend from AWS Cost Explorer.
//
// It reports totals for all RDS charges in the scanned account and regions,
// broken down by usage type. It does not attribute cost to individual
// instances: resource-level data for RDS is normally not enabled in member
// accounts (spike S1).
//
// Amounts are exact decimals (math/big), never floats. If any page of a
// request fails, that figure is unavailable: a subtotal is never shown.
package cost

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/smithy-go"
)

// Metric used for every figure: Reserved Instance fees are spread across
// the usage they cover.
const Metric = "AmortizedCost"

// Service is the Cost Explorer SERVICE dimension value for RDS.
const Service = "Amazon Relational Database Service"

// Reason explains why cost is unavailable. Fixed values only.
type Reason string

// Reasons.
const (
	ReasonNotRequested         Reason = "not_requested"
	ReasonPartitionUnsupported Reason = "partition_not_supported"
	ReasonAccessDenied         Reason = "access_denied"
	ReasonNotEnabled           Reason = "cost_explorer_not_enabled"
	ReasonThrottled            Reason = "throttled"
	ReasonTimeout              Reason = "timeout"
	ReasonAPIError             Reason = "api_error"
	ReasonMixedCurrency        Reason = "mixed_currency"
	ReasonDataUnavailable      Reason = "data_unavailable"
)

var reasonMessages = map[Reason]string{
	ReasonNotRequested:         "skipped (--no-cost)",
	ReasonPartitionUnsupported: "Cost Explorer is only supported in the commercial AWS partition",
	ReasonAccessDenied:         "access denied (needs ce:GetCostAndUsage)",
	ReasonNotEnabled:           "Cost Explorer is not enabled for this account",
	ReasonThrottled:            "Cost Explorer throttled the request",
	ReasonTimeout:              "timed out",
	ReasonAPIError:             "the Cost Explorer API returned an error",
	ReasonMixedCurrency:        "costs were returned in more than one currency, or without one",
	ReasonDataUnavailable:      "Cost Explorer has no data for this period yet",
}

// Message is the fixed, printable explanation.
func (r Reason) Message() string { return reasonMessages[r] }

// Bucket groups usage types into what a reader cares about.
type Bucket string

// Buckets, in display order.
const (
	BucketInstances Bucket = "instances"
	BucketMultiAZ   Bucket = "multi_az"
	BucketStorage   Bucket = "storage"
	BucketBackup    Bucket = "backup"
	BucketProxy     Bucket = "proxy"
	BucketOther     Bucket = "other"
)

// Buckets lists every bucket in display order.
var Buckets = []Bucket{BucketInstances, BucketMultiAZ, BucketStorage, BucketBackup, BucketProxy, BucketOther}

// BucketOf classifies a Cost Explorer usage type such as
// "USW2-InstanceUsage:db.m7g.large" or "EU-RDS:Multi-AZ-GP3-Storage".
//
// The buckets cover the common RDS usage types; anything not recognised goes
// to "other", so the buckets always add up to the total.
func BucketOf(usageType string) Bucket {
	u := usageType
	// Drop the region prefix ("USW2-", "EU-") when present.
	if i := strings.IndexByte(u, '-'); i > 0 && i <= 5 && strings.ToUpper(u[:i]) == u[:i] {
		u = u[i+1:]
	}
	switch {
	case strings.HasPrefix(u, "Multi-AZUsage") || strings.HasPrefix(u, "Multi-AZClusterUsage") ||
		strings.HasPrefix(u, "MirrorUsage") || strings.HasPrefix(u, "AlwaysOnUsage"):
		// Standby, mirror and Always On replicas are the cost of high availability.
		return BucketMultiAZ
	case strings.HasPrefix(u, "InstanceUsage") || strings.HasPrefix(u, "HeavyUsage") ||
		strings.HasPrefix(u, "BundleUsage") || strings.Contains(u, "ServerlessUsage") || strings.Contains(u, "ServerlessV2Usage"):
		return BucketInstances
	case strings.Contains(u, "Backup"):
		return BucketBackup
	case strings.Contains(u, "Proxy"):
		return BucketProxy
	case strings.Contains(u, "Storage") || strings.Contains(u, "IOPS") || strings.Contains(u, "Throughput"):
		return BucketStorage
	default:
		return BucketOther
	}
}

// Period is one costed time range.
type Period struct {
	Label     string // e.g. "May–Oct 2026"
	Start     string // inclusive, YYYY-MM-DD
	End       string // exclusive, YYYY-MM-DD
	Total     *big.Rat
	Breakdown map[Bucket]*big.Rat
	Estimated bool
}

// Result is what the report needs.
type Result struct {
	Available bool
	Reason    Reason
	Currency  string
	Months    *Period // last six complete months
	Last30    *Period // last 30 days up to today (exclusive)
}

// API is the part of the Cost Explorer client used here.
type API interface {
	GetCostAndUsage(context.Context, *costexplorer.GetCostAndUsageInput, ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error)
}

// NewClient returns a Cost Explorer client. Cost Explorer has a single
// endpoint in us-east-1 for the commercial partition, whatever region the
// scan uses.
func NewClient(cfg aws.Config) API {
	cfg = cfg.Copy()
	cfg.Region = "us-east-1"
	return costexplorer.NewFromConfig(cfg)
}

// Periods returns the six complete months before today's month and the 30
// days before today, as [start, end) dates in UTC.
func Periods(now time.Time) (months, last30 [2]time.Time) {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	months = [2]time.Time{thisMonth.AddDate(0, -6, 0), thisMonth}
	last30 = [2]time.Time{today.AddDate(0, 0, -30), today}
	return months, last30
}

// fetchTimeout bounds all Cost Explorer calls in one Fetch.
var fetchTimeout = 60 * time.Second

// Fetch reads RDS cost for the account and regions. It never returns an
// error: failures are a Result with Available false and a Reason.
func Fetch(ctx context.Context, api API, partition, account string, regions []string, now time.Time) *Result {
	if partition != "aws" {
		return &Result{Reason: ReasonPartitionUnsupported}
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	m, d := Periods(now)
	months, err := query(ctx, api, account, regions, m, types.GranularityMonthly)
	if err != nil {
		return &Result{Reason: reasonFor(ctx, err)}
	}
	last30, err := query(ctx, api, account, regions, d, types.GranularityDaily)
	if err != nil {
		return &Result{Reason: reasonFor(ctx, err)}
	}
	currency := months.currency
	if currency == "" {
		currency = last30.currency
	}
	if months.mixed || last30.mixed || (months.currency != "" && last30.currency != "" && months.currency != last30.currency) {
		return &Result{Reason: ReasonMixedCurrency}
	}
	if currency == "" {
		currency = "USD" // no rows at all: zero spend
	}
	months.period.Label = monthsLabel(m)
	last30.period.Label = daysLabel(d)
	return &Result{Available: true, Currency: currency, Months: &months.period, Last30: &last30.period}
}

type queryResult struct {
	period   Period
	currency string
	mixed    bool
}

func query(ctx context.Context, api API, account string, regions []string, span [2]time.Time, g types.Granularity) (*queryResult, error) {
	start, end := span[0].Format(time.DateOnly), span[1].Format(time.DateOnly)
	in := &costexplorer.GetCostAndUsageInput{
		TimePeriod:  &types.DateInterval{Start: aws.String(start), End: aws.String(end)},
		Granularity: g,
		Metrics:     []string{Metric},
		Filter: &types.Expression{And: []types.Expression{
			{Dimensions: &types.DimensionValues{Key: types.DimensionLinkedAccount, Values: []string{account}}},
			{Dimensions: &types.DimensionValues{Key: types.DimensionService, Values: []string{Service}}},
			{Dimensions: &types.DimensionValues{Key: types.DimensionRegion, Values: regions}},
		}},
		GroupBy: []types.GroupDefinition{{Type: types.GroupDefinitionTypeDimension, Key: aws.String(string(types.DimensionUsageType))}},
	}
	r := &queryResult{period: Period{Start: start, End: end, Total: new(big.Rat), Breakdown: map[Bucket]*big.Rat{}}}
	for _, b := range Buckets {
		r.period.Breakdown[b] = new(big.Rat)
	}
	for {
		out, err := api.GetCostAndUsage(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, t := range out.ResultsByTime {
			if t.Estimated {
				r.period.Estimated = true
			}
			for _, grp := range t.Groups {
				mv, ok := grp.Metrics[Metric]
				if !ok || len(grp.Keys) == 0 {
					continue
				}
				amount, ok := new(big.Rat).SetString(aws.ToString(mv.Amount))
				if !ok {
					return nil, errors.New("unparseable amount")
				}
				unit := aws.ToString(mv.Unit)
				switch {
				case unit == "" && amount.Sign() != 0:
					r.mixed = true // an amount without a currency cannot be added up
				case unit != "":
					if r.currency != "" && r.currency != unit {
						r.mixed = true
					}
					r.currency = unit
				}
				r.period.Total.Add(r.period.Total, amount)
				b := BucketOf(grp.Keys[0])
				r.period.Breakdown[b].Add(r.period.Breakdown[b], amount)
			}
		}
		if aws.ToString(out.NextPageToken) == "" {
			return r, nil
		}
		in.NextPageToken = out.NextPageToken
	}
}

func reasonFor(ctx context.Context, err error) Reason {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ReasonTimeout
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code, msg := apiErr.ErrorCode(), strings.ToLower(apiErr.ErrorMessage())
		switch {
		case code == "AccessDeniedException" && strings.Contains(msg, "not enabled"):
			return ReasonNotEnabled
		case code == "AccessDeniedException" || code == "AccessDenied" || code == "UnauthorizedOperation":
			return ReasonAccessDenied
		case code == "DataUnavailableException":
			return ReasonDataUnavailable
		case code == "LimitExceededException" || code == "ThrottlingException" || code == "RequestLimitExceeded":
			return ReasonThrottled
		}
	}
	return ReasonAPIError
}

func monthsLabel(span [2]time.Time) string {
	first, last := span[0], span[1].AddDate(0, -1, 0)
	if first.Year() == last.Year() {
		return fmt.Sprintf("%s–%s %d", first.Format("Jan"), last.Format("Jan"), last.Year())
	}
	return fmt.Sprintf("%s %d–%s %d", first.Format("Jan"), first.Year(), last.Format("Jan"), last.Year())
}

func daysLabel(span [2]time.Time) string {
	first, last := span[0], span[1].AddDate(0, 0, -1)
	if first.Year() == last.Year() {
		return fmt.Sprintf("%s–%s %d", first.Format("2 Jan"), last.Format("2 Jan"), last.Year())
	}
	return fmt.Sprintf("%s %d–%s %d", first.Format("2 Jan"), first.Year(), last.Format("2 Jan"), last.Year())
}

// Decimal renders an exact amount as a plain decimal string with no
// trailing zeros, e.g. "1234.5". Amounts are sums of decimal strings, so
// they always have a finite decimal form; nothing is rounded.
func Decimal(r *big.Rat) string {
	digits := 0
	ten := big.NewInt(10)
	denom := new(big.Int).Set(r.Denom())
	scale := big.NewInt(1)
	// Smallest n with denom | 10^n. Bounded for safety; Cost Explorer amounts
	// have far fewer digits.
	for new(big.Int).Mod(scale, denom).Sign() != 0 && digits < 64 {
		scale.Mul(scale, ten)
		digits++
	}
	s := r.FloatString(digits)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}

// Money renders an amount rounded to cents with thousands separators, e.g.
// "$2,140.17" for USD, "2,140.17 EUR" otherwise.
func Money(r *big.Rat, currency string) string {
	s := r.FloatString(2)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	n := b.String() + "." + frac
	sign := ""
	if neg && n != "0.00" {
		sign = "-"
	}
	if currency == "USD" {
		return sign + "$" + n
	}
	return sign + n + " " + currency
}

// SortedBuckets returns buckets with a non-zero amount, largest first.
func SortedBuckets(p *Period) []Bucket {
	var out []Bucket
	for _, b := range Buckets {
		if p.Breakdown[b].Sign() != 0 {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return p.Breakdown[out[i]].Cmp(p.Breakdown[out[j]]) > 0 })
	return out
}
