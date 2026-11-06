package cost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

var now = time.Date(2026, 11, 6, 10, 42, 0, 0, time.UTC)

type row struct{ usage, amount string }

// fakeCE serves GetCostAndUsage (awsJson1_1) for the real SDK client.
type fakeCE struct {
	mu       sync.Mutex
	requests []map[string]any
	// pages per granularity; each page is a list of rows
	pages     map[string][][]row
	unit      string
	estimated bool
	failCode  string
	failMsg   string
	failPage  int // -1: never
	// unitByGranularity overrides unit per request granularity.
	unitByGranularity map[string]string
	failGranularity   string // only fail requests with this granularity
	estimatedPage     int    // mark only this page estimated (with estimated)
	splitResults      bool   // one ResultsByTime entry per row
	signingRegions    []string
	slow              time.Duration
}

func (f *fakeCE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.signingRegions = append(f.signingRegions, signingRegion(r.Header.Get("Authorization")))
	f.mu.Unlock()
	if f.slow > 0 {
		time.Sleep(f.slow)
	}

	page := 0
	if tok, ok := req["NextPageToken"].(string); ok {
		_, _ = fmt.Sscanf(tok, "page-%d", &page)
	}
	gran0, _ := req["Granularity"].(string)
	if f.failCode != "" && page == f.failPage && (f.failGranularity == "" || f.failGranularity == gran0) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(400)
		_, _ = fmt.Fprintf(w, `{"__type":"%s","Message":%q}`, f.failCode, f.failMsg)
		return
	}
	gran, _ := req["Granularity"].(string)
	pages := f.pages[gran]
	unit := f.unit
	if u, ok := f.unitByGranularity[gran]; ok {
		unit = u
	}
	var groups []map[string]any
	if page < len(pages) {
		for _, rw := range pages[page] {
			groups = append(groups, map[string]any{
				"Keys":    []string{rw.usage},
				"Metrics": map[string]any{Metric: map[string]string{"Amount": rw.amount, "Unit": unit}},
			})
		}
	}
	estimated := f.estimated && (f.estimatedPage == 0 || f.estimatedPage == page)
	results := []map[string]any{{"TimePeriod": req["TimePeriod"], "Total": map[string]any{}, "Groups": groups, "Estimated": estimated}}
	if f.splitResults {
		results = nil
		for _, g := range groups {
			results = append(results, map[string]any{"TimePeriod": req["TimePeriod"], "Total": map[string]any{}, "Groups": []any{g}, "Estimated": estimated})
		}
	}
	resp := map[string]any{"ResultsByTime": results}
	if page+1 < len(pages) {
		resp["NextPageToken"] = fmt.Sprintf("page-%d", page+1)
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(resp)
}

func client(t *testing.T, f *fakeCE) API {
	t.Helper()
	if f.unit == "" {
		f.unit = "USD"
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return NewClient(aws.Config{
		Region:           "eu-west-1", // NewClient must override this
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint:     aws.String(srv.URL),
		RetryMaxAttempts: 1,
	})
}

func dec(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s)
	return r
}

func TestFetchTotalsAndBreakdown(t *testing.T) {
	f := &fakeCE{failPage: -1, pages: map[string][][]row{
		"MONTHLY": {
			{{"USW2-InstanceUsage:db.m7g.large", "100.10"}, {"USW2-Multi-AZUsage:db.t3.micro", "50.05"}},
			{{"USW2-RDS:GP3-Storage", "0.1"}, {"USW2-RDS:GP3-Storage", "0.2"}, {"USW2-RDS:ChargedBackupUsage", "1"}},
		},
		"DAILY": {
			{{"USW2-InstanceUsage:db.m7g.large", "10"}, {"USW2-RDS:ProxyUsage", "2.5"}, {"USW2-DataTransfer-Out-Bytes", "0.0001"}},
		},
	}}
	res := Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"us-west-2"}, now)
	if !res.Available || res.Currency != "USD" {
		t.Fatalf("%+v", res)
	}
	m := res.Months
	if m.Start != "2026-05-01" || m.End != "2026-11-01" || m.Label != "May–Oct 2026" {
		t.Errorf("months period: %s %s %q", m.Start, m.End, m.Label)
	}
	if Decimal(m.Total) != "151.45" {
		t.Errorf("months total = %s (pages must be summed exactly: 0.1+0.2 is 0.3)", Decimal(m.Total))
	}
	if Decimal(m.Breakdown[BucketStorage]) != "0.3" || Decimal(m.Breakdown[BucketInstances]) != "100.1" ||
		Decimal(m.Breakdown[BucketMultiAZ]) != "50.05" || Decimal(m.Breakdown[BucketBackup]) != "1" {
		t.Errorf("months breakdown: %v", m.Breakdown)
	}
	d := res.Last30
	if d.Start != "2026-10-07" || d.End != "2026-11-06" || d.Label != "7 Oct–5 Nov 2026" {
		t.Errorf("last30 period: %s %s %q", d.Start, d.End, d.Label)
	}
	if Decimal(d.Total) != "12.5001" || Decimal(d.Breakdown[BucketProxy]) != "2.5" || Decimal(d.Breakdown[BucketOther]) != "0.0001" {
		t.Errorf("last30: %s %v", Decimal(d.Total), d.Breakdown)
	}
}

func TestRequestFilters(t *testing.T) {
	f := &fakeCE{failPage: -1, pages: map[string][][]row{}}
	Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"us-west-2", "eu-west-1"}, now)
	if len(f.requests) != 2 {
		t.Fatalf("requests = %d", len(f.requests))
	}
	b, _ := json.Marshal(f.requests[0])
	got := string(b)
	for _, want := range []string{
		`"Key":"LINKED_ACCOUNT","Values":["123456789012"]`,
		`"Key":"SERVICE","Values":["Amazon Relational Database Service"]`,
		`"Key":"REGION","Values":["us-west-2","eu-west-1"]`,
		`"Metrics":["AmortizedCost"]`,
		`"Key":"USAGE_TYPE"`,
		`"Granularity":"MONTHLY"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("request missing %s:\n%s", want, got)
		}
	}
}

func TestNoRowsIsZeroNotMissing(t *testing.T) {
	f := &fakeCE{failPage: -1, pages: map[string][][]row{}}
	res := Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"us-west-2"}, now)
	if !res.Available || res.Months.Total.Sign() != 0 || Money(res.Months.Total, res.Currency) != "$0.00" {
		t.Errorf("%+v", res)
	}
}

func TestEstimatedFlag(t *testing.T) {
	f := &fakeCE{failPage: -1, estimated: true, pages: map[string][][]row{"DAILY": {{{"USW2-InstanceUsage:x", "1"}}}}}
	res := Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"us-west-2"}, now)
	if !res.Last30.Estimated {
		t.Error("estimated flag lost")
	}
}

func TestFailuresMakeCostUnavailable(t *testing.T) {
	twoPages := map[string][][]row{"MONTHLY": {{{"USW2-InstanceUsage:x", "1"}}, {{"USW2-InstanceUsage:x", "2"}}}}
	tests := []struct {
		name, code, msg string
		page            int
		want            Reason
	}{
		{"access denied", "AccessDeniedException", "User is not authorized", 0, ReasonAccessDenied},
		{"not enabled", "AccessDeniedException", "User not enabled for cost explorer access", 0, ReasonNotEnabled},
		{"data unavailable", "DataUnavailableException", "no data", 0, ReasonDataUnavailable},
		{"throttled", "LimitExceededException", "slow down", 0, ReasonThrottled},
		{"later page fails", "InternalServerException", "boom", 1, ReasonAPIError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeCE{pages: twoPages, failCode: tt.code, failMsg: tt.msg, failPage: tt.page}
			res := Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"us-west-2"}, now)
			if res.Available || res.Reason != tt.want || res.Months != nil || res.Last30 != nil {
				t.Errorf("got %+v, want unavailable with %s and no partial figures", res, tt.want)
			}
		})
	}
}

func TestOtherPartitionsMakeNoCalls(t *testing.T) {
	f := &fakeCE{failPage: -1}
	res := Fetch(context.Background(), client(t, f), "aws-us-gov", "123456789012", []string{"us-gov-west-1"}, now)
	if res.Available || res.Reason != ReasonPartitionUnsupported || len(f.requests) != 0 {
		t.Errorf("%+v, %d requests", res, len(f.requests))
	}
}

func TestCurrency(t *testing.T) {
	f := &fakeCE{failPage: -1, unit: "EUR", pages: map[string][][]row{"MONTHLY": {{{"x", "1"}}}}}
	res := Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"eu-west-1"}, now)
	if !res.Available || res.Currency != "EUR" || Money(res.Months.Total, res.Currency) != "1.00 EUR" {
		t.Errorf("%+v", res)
	}

	f = &fakeCE{failPage: -1, unitByGranularity: map[string]string{"MONTHLY": "USD", "DAILY": "EUR"},
		pages: map[string][][]row{"MONTHLY": {{{"x", "1"}}}, "DAILY": {{{"x", "1"}}}}}
	res = Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"eu-west-1"}, now)
	if res.Available || res.Reason != ReasonMixedCurrency {
		t.Errorf("mixed currencies must not be added up: %+v", res)
	}
}

func TestPeriodsAcrossYears(t *testing.T) {
	m, d := Periods(time.Date(2027, 1, 3, 23, 59, 0, 0, time.UTC))
	if m[0].Format(time.DateOnly) != "2026-07-01" || m[1].Format(time.DateOnly) != "2027-01-01" {
		t.Errorf("months = %v", m)
	}
	if monthsLabel(m) != "Jul–Dec 2026" {
		t.Errorf("label = %q", monthsLabel(m))
	}
	if d[0].Format(time.DateOnly) != "2026-12-04" || d[1].Format(time.DateOnly) != "2027-01-03" || daysLabel(d) != "4 Dec 2026–2 Jan 2027" {
		t.Errorf("days = %v %q", d, daysLabel(d))
	}
	// A local time just after midnight UTC must still use the UTC date.
	loc := time.FixedZone("UTC-5", -5*3600)
	_, d = Periods(time.Date(2026, 11, 5, 20, 0, 0, 0, loc)) // 01:00 UTC on 6 Nov
	if d[1].Format(time.DateOnly) != "2026-11-06" {
		t.Errorf("end = %v", d[1])
	}
}

func TestBucketOf(t *testing.T) {
	for in, want := range map[string]Bucket{
		"USW2-InstanceUsage:db.m7g.large": BucketInstances,
		"InstanceUsage:db.t3.micro":       BucketInstances,
		"USE1-HeavyUsage:db.r6g.large":    BucketInstances,
		"EU-Multi-AZUsage:db.t4g.micro":   BucketMultiAZ,
		"USW2-RDS:Multi-AZ-GP3-Storage":   BucketStorage,
		"USW2-RDS:GP2-Storage":            BucketStorage,
		"USW2-RDS:PIOPS":                  BucketStorage,
		"USW2-RDS:GP3-Throughput":         BucketStorage,
		"EU-RDS:ChargedBackupUsage":       BucketBackup,
		"USW2-RDS:ProxyUsage":             BucketProxy,
		"USW2-USE1-AWS-Out-Bytes":         BucketOther,
		"USW2-DataTransfer-Out-Bytes":     BucketOther,
		"USW2-RDS:PI_API":                 BucketOther,
	} {
		if got := BucketOf(in); got != want {
			t.Errorf("BucketOf(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestMoneyAndDecimal(t *testing.T) {
	for in, want := range map[string]string{
		"0": "$0.00", "1": "$1.00", "1234.5": "$1,234.50", "1234567.891": "$1,234,567.89",
		"0.004": "$0.00", "0.005": "$0.01", "-12.5": "-$12.50", "-0.001": "$0.00", "999.999": "$1,000.00",
	} {
		if got := Money(dec(in), "USD"); got != want {
			t.Errorf("Money(%s) = %s, want %s", in, got, want)
		}
	}
	if got := Money(dec("-1234.5"), "EUR"); got != "-1,234.50 EUR" {
		t.Errorf("EUR: %s", got)
	}
	for in, want := range map[string]string{"151.4500": "151.45", "0.3": "0.3", "12": "12", "0.0001": "0.0001", "-0": "0"} {
		if got := Decimal(dec(in)); got != want {
			t.Errorf("Decimal(%s) = %s, want %s", in, got, want)
		}
	}
}

func signingRegion(auth string) string {
	i := strings.Index(auth, "Credential=")
	if i < 0 {
		return ""
	}
	parts := strings.Split(auth[i+len("Credential="):], "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func TestClientSignsForUSEast1(t *testing.T) {
	f := &fakeCE{failPage: -1}
	Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"eu-west-1"}, now)
	for _, r := range f.signingRegions {
		if r != "us-east-1" {
			t.Errorf("signed for %q, want us-east-1", r)
		}
	}
}

func TestMultipleResultsAndLaterEstimatedPage(t *testing.T) {
	f := &fakeCE{failPage: -1, splitResults: true, estimated: true, estimatedPage: 1, pages: map[string][][]row{
		"DAILY": {{{"USW2-InstanceUsage:x", "1"}, {"USW2-InstanceUsage:x", "2"}}, {{"USW2-RDS:GP3-Storage", "3"}}},
	}}
	res := Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"us-west-2"}, now)
	if !res.Available || Decimal(res.Last30.Total) != "6" || !res.Last30.Estimated || res.Months.Estimated {
		t.Errorf("%+v / %+v", res.Last30, res.Months)
	}
}

func TestDailyFailureAfterMonthlySucceeds(t *testing.T) {
	f := &fakeCE{failCode: "InternalServerException", failPage: 0, failGranularity: "DAILY",
		pages: map[string][][]row{"MONTHLY": {{{"x", "1"}}}}}
	res := Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"us-west-2"}, now)
	if res.Available || res.Months != nil || res.Reason != ReasonAPIError {
		t.Errorf("a later request failing must make all cost unavailable: %+v", res)
	}
}

func TestMalformedAmountAndMissingUnit(t *testing.T) {
	f := &fakeCE{failPage: -1, pages: map[string][][]row{"MONTHLY": {{{"x", "not-a-number"}}}}}
	if res := Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"us-west-2"}, now); res.Available || res.Reason != ReasonAPIError {
		t.Errorf("malformed: %+v", res)
	}
	f = &fakeCE{failPage: -1, unitByGranularity: map[string]string{"MONTHLY": ""}, unit: "USD", pages: map[string][][]row{"MONTHLY": {{{"x", "5"}}}}}
	if res := Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"us-west-2"}, now); res.Available || res.Reason != ReasonMixedCurrency {
		t.Errorf("missing unit: %+v", res)
	}
}

func TestTimeout(t *testing.T) {
	old := fetchTimeout
	fetchTimeout = 100 * time.Millisecond
	t.Cleanup(func() { fetchTimeout = old })
	f := &fakeCE{failPage: -1, slow: 500 * time.Millisecond}
	if res := Fetch(context.Background(), client(t, f), "aws", "123456789012", []string{"us-west-2"}, now); res.Available || res.Reason != ReasonTimeout {
		t.Errorf("%+v", res)
	}
}

func TestDecimalIsLossless(t *testing.T) {
	sum := new(big.Rat)
	for _, s := range []string{"0.00000000001", "0.1", "0.2", "1234.567890123456789"} {
		sum.Add(sum, dec(s))
	}
	if got := Decimal(sum); got != "1234.867890123466789" {
		t.Errorf("Decimal = %s", got)
	}
	if got := Decimal(dec("0.00000000001")); got != "0.00000000001" {
		t.Errorf("Decimal = %s", got)
	}
}
