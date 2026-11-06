package discover

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

const account = "123456789012"

// fakeAWS serves STS GetCallerIdentity and RDS DescribeDBInstances (query
// protocol, XML) for the real SDK clients.
type fakeAWS struct {
	mu       sync.Mutex
	pages    map[string][]string // region -> pages of <DBInstance> XML
	failPage map[string]int      // region -> page index that returns failCode
	failCode string
	stsCode  string
	calls    map[string]int
	slow     time.Duration
	failTags bool
	inflight int
	maxSeen  int
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	region := regionFromAuth(r.Header.Get("Authorization"))
	f.mu.Lock()
	f.calls[r.Form.Get("Action")+"/"+region]++
	f.inflight++
	if f.inflight > f.maxSeen {
		f.maxSeen = f.inflight
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inflight--; f.mu.Unlock() }()
	if f.slow > 0 {
		time.Sleep(f.slow)
	}
	switch r.Form.Get("Action") {
	case "GetCallerIdentity":
		if f.stsCode != "" {
			writeError(w, f.stsCode)
			return
		}
		_, _ = fmt.Fprintf(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>
<Arn>arn:aws:iam::%s:user/tester</Arn><UserId>AIDA</UserId><Account>%s</Account></GetCallerIdentityResult>
<ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`, account, account)
	case "DescribeDBInstances":
		page := 0
		if m := r.Form.Get("Marker"); m != "" {
			_, _ = fmt.Sscanf(m, "page-%d", &page)
		}
		if fp, ok := f.failPage[region]; ok && fp == page {
			writeError(w, f.failCode)
			return
		}
		pages := f.pages[region]
		body := ""
		if page < len(pages) {
			body = pages[page]
		}
		marker := ""
		if page+1 < len(pages) {
			marker = fmt.Sprintf("<Marker>page-%d</Marker>", page+1)
		}
		_, _ = fmt.Fprintf(w, `<DescribeDBInstancesResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/"><DescribeDBInstancesResult>
<DBInstances>%s</DBInstances>%s</DescribeDBInstancesResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></DescribeDBInstancesResponse>`, body, marker)
	case "ListTagsForResource":
		if f.failTags {
			writeError(w, "AccessDenied")
			return
		}
		_, _ = fmt.Fprint(w, `<ListTagsForResourceResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/"><ListTagsForResourceResult>
<TagList><Tag><Key>from</Key><Value>fallback</Value></Tag></TagList></ListTagsForResourceResult>
<ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></ListTagsForResourceResponse>`)
	default:
		http.Error(w, "unexpected action", 400)
	}
}

func writeError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(403)
	_, _ = fmt.Fprintf(w, `<ErrorResponse><Error><Type>Sender</Type><Code>%s</Code><Message>OOS-SECRET-MARKER details for arn:aws:iam::%s:user/x</Message></Error><RequestId>1</RequestId></ErrorResponse>`, code, account)
}

// regionFromAuth reads the region from the SigV4 credential scope.
func regionFromAuth(h string) string {
	i := strings.Index(h, "Credential=")
	if i < 0 {
		return ""
	}
	parts := strings.Split(h[i+len("Credential="):], "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func db(name, rid string, extra string) string {
	return fmt.Sprintf(`<DBInstance><DBInstanceIdentifier>%s</DBInstanceIdentifier>
<DBInstanceArn>arn:aws:rds:eu-west-1:%s:db:%s</DBInstanceArn><DbiResourceId>%s</DbiResourceId>
<Engine>postgres</Engine><EngineVersion>16.4</EngineVersion><DBInstanceClass>db.t4g.micro</DBInstanceClass>
<DBInstanceStatus>available</DBInstanceStatus><InstanceCreateTime>2025-03-02T10:00:00Z</InstanceCreateTime>%s</DBInstance>`,
		name, account, name, rid, extra)
}

func newTestAWS(t *testing.T, f *fakeAWS) *AWS {
	t.Helper()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := aws.Config{
		Region:           "eu-west-1",
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint:     aws.String(srv.URL),
		RetryMaxAttempts: 1,
	}
	return newAWS(cfg)
}

func TestDiscoverPaginatesAndMaps(t *testing.T) {
	f := &fakeAWS{pages: map[string][]string{
		"eu-west-1": {
			db("orders-db", "db-ORDERS", `<TagList><Tag><Key>team</Key><Value>data</Value></Tag></TagList>`),
			db("aurora-1", "db-AURORA", `<DBClusterIdentifier>billing</DBClusterIdentifier>`) + db("docs", "db-DOCS", ""),
		},
		"us-east-1": {""},
	}}
	inv, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1", "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Partition != "aws" || inv.AccountID != account || !inv.Complete() {
		t.Fatalf("inventory: %+v", inv)
	}
	got := inv.Instances()
	if len(got) != 3 {
		t.Fatalf("instances = %d, want 3 (both pages)", len(got))
	}
	o := got[0]
	if o.Identifier != "orders-db" || o.ResourceID != "db-ORDERS" || o.Region != "eu-west-1" ||
		o.AccountID != account || o.Tags["team"] != "data" || o.Created.Year() != 2025 || o.NotCovered != "" {
		t.Errorf("orders-db: %+v", o)
	}
	if got[1].NotCovered != NotCoveredClusterMember {
		t.Errorf("aurora-1 should be not covered: %+v", got[1])
	}
	if f.calls["DescribeDBInstances/eu-west-1"] != 2 || f.calls["DescribeDBInstances/us-east-1"] != 1 {
		t.Errorf("calls: %v", f.calls)
	}
}

func TestLaterPageFailureFailsTheRegion(t *testing.T) {
	f := &fakeAWS{
		pages:    map[string][]string{"eu-west-1": {db("a", "db-A", ""), db("b", "db-B", "")}, "us-east-1": {db("c", "db-C", "")}},
		failPage: map[string]int{"eu-west-1": 1},
		failCode: "Throttling",
	}
	inv, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1", "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	eu := inv.Regions[0]
	if !eu.Failed || eu.Error != ErrThrottled || len(eu.Instances) != 0 {
		t.Errorf("eu-west-1 must fail with no partial instances: %+v", eu)
	}
	if inv.Regions[1].Failed || len(inv.Regions[1].Instances) != 1 {
		t.Errorf("us-east-1 should be unaffected: %+v", inv.Regions[1])
	}
	if inv.Complete() {
		t.Error("inventory must be incomplete")
	}
}

func TestRegionErrorCategories(t *testing.T) {
	for code, want := range map[string]ErrorCategory{
		"AccessDenied":          ErrAccessDenied,
		"AccessDeniedException": ErrAccessDenied,
		"Throttling":            ErrThrottled,
		"InvalidClientTokenId":  ErrRegionDisabled,
		"OptInRequired":         ErrRegionDisabled,
		"InternalFailure":       ErrAPI,
	} {
		f := &fakeAWS{pages: map[string][]string{}, failPage: map[string]int{"eu-west-1": 0}, failCode: code}
		inv, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"})
		if err != nil {
			t.Fatal(err)
		}
		if got := inv.Regions[0].Error; got != want {
			t.Errorf("%s: category %s, want %s", code, got, want)
		}
	}
}

func TestIdentityFailureIsFatal(t *testing.T) {
	for code, want := range map[string]ErrorCategory{
		"InvalidClientTokenId": ErrNoCredentials,
		"ExpiredToken":         ErrNoCredentials,
		"AccessDenied":         ErrAccessDenied,
	} {
		f := &fakeAWS{stsCode: code}
		_, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"})
		var fe *FatalError
		if !errors.As(err, &fe) || fe.Category != want {
			t.Errorf("%s: err = %v, want %s", code, err, want)
			continue
		}
		if strings.Contains(fe.Error(), "OOS-SECRET-MARKER") || strings.Contains(fe.Error(), "user/x") {
			t.Errorf("fatal error leaks AWS error text: %q", fe.Error())
		}
	}
}

func TestRegionTimeout(t *testing.T) {
	f := &fakeAWS{pages: map[string][]string{"eu-west-1": {db("a", "db-A", "")}}}
	a := newTestAWS(t, f)
	a.regionTimeout = 100 * time.Millisecond
	f.slow = 500 * time.Millisecond
	// STS is slow too, but has no deadline of its own here; give it room.
	ctx := context.Background()
	inv, err := a.Discover(ctx, []string{"eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Regions[0].Failed || inv.Regions[0].Error != ErrTimeout {
		t.Errorf("region: %+v", inv.Regions[0])
	}
}

func TestNoRegions(t *testing.T) {
	_, err := newTestAWS(t, &fakeAWS{}).Discover(context.Background(), nil)
	var fe *FatalError
	if !errors.As(err, &fe) || fe.Category != ErrNoRegion {
		t.Errorf("err = %v", err)
	}
}

func TestPartitionOf(t *testing.T) {
	for in, want := range map[string]string{
		"arn:aws:iam::123456789012:user/x":                  "aws",
		"arn:aws-us-gov:sts::123456789012:assumed-role/r/s": "aws-us-gov",
		"nonsense": "",
		"":         "",
	} {
		if got := partitionOf(in); got != want {
			t.Errorf("partitionOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTagFallback(t *testing.T) {
	withTags := db("a", "db-A", `<TagList><Tag><Key>team</Key><Value>data</Value></Tag></TagList>`)
	emptyTags := db("b", "db-B", `<TagList></TagList>`)
	noTagList := db("c", "db-C", "")

	f := &fakeAWS{pages: map[string][]string{"eu-west-1": {withTags + emptyTags + noTagList}}}
	inv, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	got := inv.Instances()
	if got[0].Tags["team"] != "data" || got[0].TagsUnavailable {
		t.Errorf("TagList present: %+v", got[0])
	}
	if got[2].Tags["from"] != "fallback" || got[2].TagsUnavailable {
		t.Errorf("fallback: %+v", got[2])
	}
	if f.calls["ListTagsForResource/eu-west-1"] != 1 {
		t.Errorf("ListTagsForResource calls = %d, want 1 (only for the missing TagList)", f.calls["ListTagsForResource/eu-west-1"])
	}

	f = &fakeAWS{pages: map[string][]string{"eu-west-1": {noTagList}}, failTags: true}
	inv, err = newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Complete() || !inv.Instances()[0].TagsUnavailable {
		t.Errorf("a tag failure must be a warning, not a failed region: %+v", inv.Regions[0])
	}
}

func TestRegionConcurrencyIsBounded(t *testing.T) {
	regions := []string{"eu-west-1", "eu-west-2", "eu-west-3", "eu-central-1", "us-east-1", "us-east-2", "us-west-1", "us-west-2"}
	f := &fakeAWS{pages: map[string][]string{}, slow: 50 * time.Millisecond}
	inv, err := newTestAWS(t, f).Discover(context.Background(), regions)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Regions) != len(regions) || !inv.Complete() {
		t.Fatalf("%+v", inv)
	}
	for i, r := range inv.Regions {
		if r.Region != regions[i] {
			t.Errorf("region order: %d = %s, want %s", i, r.Region, regions[i])
		}
	}
	if f.maxSeen > maxConcurrentRegions {
		t.Errorf("max concurrent requests = %d, want <= %d", f.maxSeen, maxConcurrentRegions)
	}
	if f.maxSeen < 2 {
		t.Errorf("regions were not scanned concurrently (max %d)", f.maxSeen)
	}
}

func TestIdentityTimeout(t *testing.T) {
	f := &fakeAWS{slow: 500 * time.Millisecond}
	a := newTestAWS(t, f)
	a.identityTimeout = 100 * time.Millisecond
	_, err := a.Discover(context.Background(), []string{"eu-west-1"})
	var fe *FatalError
	if !errors.As(err, &fe) || fe.Category != ErrTimeout {
		t.Errorf("err = %v", err)
	}
}
