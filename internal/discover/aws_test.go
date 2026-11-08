package discover

import (
	"context"
	"encoding/json"
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
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

const account = "123456789012"

// fakeAWS serves STS GetCallerIdentity and RDS DescribeDBInstances (query
// protocol, XML) for the real SDK clients.
type fakeAWS struct {
	mu               sync.Mutex
	pages            map[string][]string // region -> pages of <DBInstance> XML
	failPage         map[string]int      // region -> page index that returns failCode
	failCode         string
	stsCode          string
	calls            map[string]int
	slow             time.Duration
	failTags         bool
	lbPages          map[string][]string          // region -> pages of <member> XML
	s3Pages          map[string][]string          // region -> pages of <Bucket> XML
	s3Tags           map[string]map[string]string // bucket -> tags; absent = NoSuchTagSet
	s3TagFail        map[string]bool              // bucket -> GetBucketTagging fails
	s3Regions        []string                     // bucket-region filter of each ListBuckets call
	s3TagStallFirst  map[string]bool              // bucket -> first GetBucketTagging call stalls
	s3TagStallAlways map[string]bool              // bucket -> every GetBucketTagging call stalls
	lbTags           map[string]map[string]string // ARN -> tags
	lambdaPages      map[string][]string          // region -> pages of function JSON
	lambdaTags       map[string]map[string]string // ARN -> tags
	lambdaTagFail    map[string]bool              // ARN -> ListTags fails
	lambdaQueries    []string                     // query string of each ListFunctions call
	tagBatches       []int                        // ARNs per DescribeTags call
	inflight         int
	maxSeen          int
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/2015-03-31/functions") || strings.HasPrefix(r.URL.Path, "/2017-03-31/tags/") {
		f.serveLambda(w, r)
		return
	}
	if r.Method == http.MethodGet {
		f.serveS3(w, r)
		return
	}
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
	case "DescribeLoadBalancers":
		page := 0
		if m := r.Form.Get("Marker"); m != "" {
			_, _ = fmt.Sscanf(m, "page-%d", &page)
		}
		if fp, ok := f.failPage["lb/"+region]; ok && fp == page {
			writeError(w, f.failCode)
			return
		}
		pages := f.lbPages[region]
		body := ""
		if page < len(pages) {
			body = pages[page]
		}
		marker := ""
		if page+1 < len(pages) {
			marker = fmt.Sprintf("<NextMarker>page-%d</NextMarker>", page+1)
		}
		_, _ = fmt.Fprintf(w, `<DescribeLoadBalancersResponse xmlns="http://elasticloadbalancing.amazonaws.com/doc/2015-12-01/"><DescribeLoadBalancersResult>
<LoadBalancers>%s</LoadBalancers>%s</DescribeLoadBalancersResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></DescribeLoadBalancersResponse>`, body, marker)
	case "DescribeTags":
		f.mu.Lock()
		f.tagBatches = append(f.tagBatches, countMembers(r.Form, "ResourceArns.member."))
		f.mu.Unlock()
		if f.failTags {
			writeError(w, "AccessDenied")
			return
		}
		var descs strings.Builder
		for i := 1; ; i++ {
			arn := r.Form.Get(fmt.Sprintf("ResourceArns.member.%d", i))
			if arn == "" {
				break
			}
			tags := ""
			for k, v := range f.lbTags[arn] {
				tags += fmt.Sprintf("<member><Key>%s</Key><Value>%s</Value></member>", k, v)
			}
			fmt.Fprintf(&descs, "<member><ResourceArn>%s</ResourceArn><Tags>%s</Tags></member>", arn, tags)
		}
		_, _ = fmt.Fprintf(w, `<DescribeTagsResponse xmlns="http://elasticloadbalancing.amazonaws.com/doc/2015-12-01/"><DescribeTagsResult>
<TagDescriptions>%s</TagDescriptions></DescribeTagsResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></DescribeTagsResponse>`, descs.String())
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

func countMembers(form map[string][]string, prefix string) int {
	n := 0
	for k := range form {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

func lbXML(name, kind, id, lbType string) (string, string) {
	arn := fmt.Sprintf("arn:aws:elasticloadbalancing:eu-west-1:%s:loadbalancer/%s/%s/%s", account, kind, name, id)
	return arn, fmt.Sprintf(`<member><LoadBalancerArn>%s</LoadBalancerArn><LoadBalancerName>%s</LoadBalancerName>
<Type>%s</Type><Scheme>internet-facing</Scheme><VpcId>vpc-123</VpcId><State><Code>active</Code></State>
<CreatedTime>2025-03-02T10:00:00Z</CreatedTime></member>`, arn, name, lbType)
}

func TestLoadBalancerDiscovery(t *testing.T) {
	var page1, page2 strings.Builder
	tags := map[string]map[string]string{}
	// 25 load balancers over two pages: tags must be fetched in batches of 20.
	for i := 0; i < 25; i++ {
		arn, x := lbXML(fmt.Sprintf("lb-%02d", i), "app", fmt.Sprintf("%016x", i+1), "application")
		if i < 15 {
			page1.WriteString(x)
		} else {
			page2.WriteString(x)
		}
		tags[arn] = map[string]string{"team": "web"}
	}
	gwArn, gw := lbXML("gw", "gwy", "00000000000000ff", "gateway")
	k8sArn, k8s := lbXML("k8s", "net", "00000000000000fe", "network")
	page2.WriteString(gw + k8s)
	tags[gwArn] = map[string]string{}
	tags[k8sArn] = map[string]string{"elbv2.k8s.aws/cluster": "prod", "service.k8s.aws/stack": "default/web"}

	f := &fakeAWS{lbPages: map[string][]string{"eu-west-1": {page1.String(), page2.String()}}, lbTags: tags}
	inv, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"}, TypeLoadBalancer)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Regions) != 1 || inv.Regions[0].Type != TypeLoadBalancer || !inv.Complete() {
		t.Fatalf("regions: %+v", inv.Regions)
	}
	got := inv.Instances()
	if len(got) != 27 {
		t.Fatalf("load balancers = %d, want 27", len(got))
	}
	first := got[0]
	if first.Type != TypeLoadBalancer || first.Kind != KindALB || first.Identifier != "lb-00" || first.Scheme != "internet-facing" ||
		first.VPCID != "vpc-123" || first.Status != "active" || first.Tags["team"] != "web" || first.Created.Year() != 2025 {
		t.Errorf("first: %+v", first)
	}
	byName := map[string]Instance{}
	for _, i := range got {
		byName[i.Identifier] = i
	}
	if byName["gw"].NotCovered != NotCoveredGatewayLB || byName["gw"].Kind != KindGWLB {
		t.Errorf("gateway: %+v", byName["gw"])
	}
	if byName["k8s"].ManagedBy != ManagedByKubernetes || byName["k8s"].Kind != KindNLB {
		t.Errorf("k8s: %+v", byName["k8s"])
	}
	for _, n := range f.tagBatches {
		if n > 20 {
			t.Errorf("DescribeTags called with %d ARNs, max is 20", n)
		}
	}
	if len(f.tagBatches) != 2 {
		t.Errorf("DescribeTags calls = %d, want 2 for 27 load balancers", len(f.tagBatches))
	}
}

func TestLoadBalancerTagFailureIsAWarning(t *testing.T) {
	_, x := lbXML("web", "app", "0000000000000001", "application")
	f := &fakeAWS{lbPages: map[string][]string{"eu-west-1": {x}}, failTags: true}
	inv, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"}, TypeLoadBalancer)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Complete() || !inv.Instances()[0].TagsUnavailable {
		t.Errorf("%+v", inv.Regions)
	}
}

func TestLoadBalancerFailureOnlyFailsThatType(t *testing.T) {
	f := &fakeAWS{
		pages:    map[string][]string{"eu-west-1": {db("orders-db", "db-ORDERS", `<TagList></TagList>`)}},
		failPage: map[string]int{"lb/eu-west-1": 0},
		failCode: "AccessDenied",
	}
	inv, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"}, TypeDBInstance, TypeLoadBalancer)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Regions) != 2 || inv.Regions[0].Type != TypeDBInstance || inv.Regions[1].Type != TypeLoadBalancer {
		t.Fatalf("%+v", inv.Regions)
	}
	if inv.Regions[0].Failed || len(inv.Regions[0].Instances) != 1 {
		t.Errorf("RDS must be unaffected: %+v", inv.Regions[0])
	}
	if !inv.Regions[1].Failed || inv.Regions[1].Error != ErrAccessDenied || inv.Complete() {
		t.Errorf("load balancers: %+v", inv.Regions[1])
	}
	if names := inv.RegionNames(); len(names) != 1 || names[0] != "eu-west-1" {
		t.Errorf("RegionNames = %v", names)
	}
}

func TestManagedBy(t *testing.T) {
	for want, tags := range map[string]map[string]string{
		ManagedByKubernetes:       {"ingress.k8s.aws/stack": "x", "aws:cloudformation:stack-name": "eks"},
		ManagedByElasticBeanstalk: {"elasticbeanstalk:environment-name": "prod"},
		ManagedByCloudFormation:   {"aws:cloudformation:stack-name": "s"},
		"":                        {"team": "web"},
	} {
		if got := managedBy(tags); got != want {
			t.Errorf("managedBy(%v) = %q, want %q", tags, got, want)
		}
	}
}

func TestLoadBalancerLaterPageFailure(t *testing.T) {
	_, a := lbXML("a", "app", "0000000000000001", "application")
	_, b := lbXML("b", "app", "0000000000000002", "application")
	f := &fakeAWS{lbPages: map[string][]string{"eu-west-1": {a, b}}, failPage: map[string]int{"lb/eu-west-1": 1}, failCode: "Throttling"}
	inv, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"}, TypeLoadBalancer)
	if err != nil {
		t.Fatal(err)
	}
	r := inv.Regions[0]
	if !r.Failed || r.Error != ErrThrottled || len(r.Instances) != 0 {
		t.Errorf("a later-page failure must fail the region with no partial list: %+v", r)
	}
}

func TestConcurrencyIsBoundedAcrossTypes(t *testing.T) {
	regions := []string{"eu-west-1", "eu-west-2", "us-east-1", "us-west-2"}
	f := &fakeAWS{pages: map[string][]string{}, lbPages: map[string][]string{}, slow: 50 * time.Millisecond}
	inv, err := newTestAWS(t, f).Discover(context.Background(), regions, TypeDBInstance, TypeLoadBalancer)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Regions) != 8 || !inv.Complete() {
		t.Fatalf("%+v", inv.Regions)
	}
	for i, r := range inv.Regions {
		wantType := []string{TypeDBInstance, TypeLoadBalancer}[i%2]
		if r.Region != regions[i/2] || r.Type != wantType {
			t.Errorf("result %d = %s/%s, want %s/%s", i, r.Region, r.Type, regions[i/2], wantType)
		}
	}
	if f.maxSeen > maxConcurrentRegions {
		t.Errorf("max concurrent requests = %d, want <= %d", f.maxSeen, maxConcurrentRegions)
	}
}

func (f *fakeAWS) serveS3(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.inflight++
	if f.inflight > f.maxSeen {
		f.maxSeen = f.inflight
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inflight--; f.mu.Unlock() }()
	if f.slow > 0 {
		time.Sleep(f.slow)
	}
	q := r.URL.Query()
	region := regionFromAuth(r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/xml")
	if r.URL.Path == "/" {
		f.mu.Lock()
		f.s3Regions = append(f.s3Regions, q.Get("bucket-region"))
		f.calls["ListBuckets/"+region]++
		f.mu.Unlock()
		page0 := 0
		if tok := q.Get("continuation-token"); tok != "" {
			_, _ = fmt.Sscanf(tok, "page-%d", &page0)
		}
		if fp, ok := f.failPage["s3/"+region]; ok && fp == page0 {
			w.WriteHeader(403)
			_, _ = fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>no</Message></Error>`)
			return
		}
		page := 0
		if tok := q.Get("continuation-token"); tok != "" {
			_, _ = fmt.Sscanf(tok, "page-%d", &page)
		}
		pages := f.s3Pages[q.Get("bucket-region")]
		body := ""
		if page < len(pages) {
			body = pages[page]
		}
		token := ""
		if page+1 < len(pages) {
			token = fmt.Sprintf("<ContinuationToken>page-%d</ContinuationToken>", page+1)
		}
		_, _ = fmt.Fprintf(w, `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>o</ID></Owner><Buckets>%s</Buckets>%s</ListAllMyBucketsResult>`, body, token)
		return
	}
	if _, ok := q["tagging"]; ok {
		bucket := strings.TrimPrefix(r.URL.Path, "/")
		f.mu.Lock()
		f.calls["GetBucketTagging/"+bucket]++
		stall := (f.s3TagStallFirst[bucket] && f.calls["GetBucketTagging/"+bucket] == 1) || f.s3TagStallAlways[bucket]
		f.mu.Unlock()
		if stall {
			time.Sleep(500 * time.Millisecond) // longer than the test's attempt deadline
		}
		if f.s3TagFail[bucket] {
			w.WriteHeader(403)
			_, _ = fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>no</Message></Error>`)
			return
		}
		tags, ok := f.s3Tags[bucket]
		if !ok {
			w.WriteHeader(404)
			_, _ = fmt.Fprint(w, `<Error><Code>NoSuchTagSet</Code><Message>The TagSet does not exist</Message></Error>`)
			return
		}
		var ts strings.Builder
		for k, v := range tags {
			fmt.Fprintf(&ts, "<Tag><Key>%s</Key><Value>%s</Value></Tag>", k, v)
		}
		_, _ = fmt.Fprintf(w, `<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet>%s</TagSet></Tagging>`, ts.String())
		return
	}
	http.Error(w, "unexpected S3 request", 400)
}

func bucketXML(name, region string) string {
	return fmt.Sprintf(`<Bucket><Name>%s</Name><CreationDate>2024-05-01T08:00:00.000Z</CreationDate><BucketRegion>%s</BucketRegion><BucketArn>arn:aws:s3:::%s</BucketArn></Bucket>`, name, region, name)
}

// pathStyle points the S3 client at the test server with path-style URLs.
func pathStyle(a *AWS) {
	a.newS3 = func(c aws.Config, region string) s3API {
		return s3.NewFromConfig(c, func(o *s3.Options) { o.Region = region; o.UsePathStyle = true })
	}
	a.newS3PathStyle = a.newS3
}

func TestS3Discovery(t *testing.T) {
	f := &fakeAWS{
		s3Pages: map[string][]string{"eu-west-1": {
			bucketXML("acme-logs", "eu-west-1") + bucketXML("cdk-hnb659fds-assets-123456789012-eu-west-1", "eu-west-1"),
			bucketXML("tagged", "eu-west-1") + bucketXML("stack-bucket", "eu-west-1") + bucketXML("no-tag-access", "eu-west-1") +
				bucketXML("elsewhere", "us-east-1"), // must be skipped even if the filter let it through
		}},
		s3Tags: map[string]map[string]string{
			"tagged":       {"team": "data"},
			"stack-bucket": {"aws:cloudformation:stack-name": "app"},
		},
		s3TagFail: map[string]bool{"no-tag-access": true},
	}
	a := newTestAWS(t, f)
	pathStyle(a)
	inv, err := a.Discover(context.Background(), []string{"eu-west-1"}, TypeS3Bucket)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Complete() || len(inv.Regions) != 1 || inv.Regions[0].Type != TypeS3Bucket {
		t.Fatalf("%+v", inv.Regions)
	}
	by := map[string]Instance{}
	for _, b := range inv.Instances() {
		by[b.Identifier] = b
	}
	if len(by) != 5 {
		t.Fatalf("buckets = %d, want 5 (two pages)", len(by))
	}
	logs := by["acme-logs"]
	if logs.Type != TypeS3Bucket || logs.Kind != KindS3 || logs.ARN != "arn:aws:s3:::acme-logs" || logs.Region != "eu-west-1" ||
		logs.AccountID != account || logs.Created.Year() != 2024 || logs.TagsUnavailable || len(logs.Tags) != 0 || logs.ManagedBy != "" {
		t.Errorf("acme-logs (no tags): %+v", logs)
	}
	if by["tagged"].Tags["team"] != "data" {
		t.Errorf("tagged: %+v", by["tagged"])
	}
	if by["stack-bucket"].ManagedBy != ManagedByCloudFormation {
		t.Errorf("stack-bucket: %+v", by["stack-bucket"])
	}
	if cdk := by["cdk-hnb659fds-assets-123456789012-eu-west-1"]; cdk.LikelyCreatedBy != ManagedByCDK || cdk.ManagedBy != "" {
		t.Errorf("cdk bucket: %+v", by["cdk-hnb659fds-assets-123456789012-eu-west-1"])
	}
	if !by["no-tag-access"].TagsUnavailable {
		t.Errorf("tag failure must be a warning: %+v", by["no-tag-access"])
	}
	for _, r := range f.s3Regions {
		if r != "eu-west-1" {
			t.Errorf("ListBuckets bucket-region = %q, want eu-west-1", r)
		}
	}
	if f.calls["ListBuckets/eu-west-1"] != 2 {
		t.Errorf("ListBuckets calls signed for eu-west-1 = %d, want 2", f.calls["ListBuckets/eu-west-1"])
	}
}

func TestS3FailureOnlyFailsThatType(t *testing.T) {
	f := &fakeAWS{pages: map[string][]string{"eu-west-1": {db("orders-db", "db-ORDERS", `<TagList></TagList>`)}},
		failPage: map[string]int{"s3/eu-west-1": 0}}
	a := newTestAWS(t, f)
	pathStyle(a)
	inv, err := a.Discover(context.Background(), []string{"eu-west-1"}, TypeDBInstance, TypeS3Bucket)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Regions[0].Failed || !inv.Regions[1].Failed || inv.Regions[1].Error != ErrAccessDenied {
		t.Errorf("%+v", inv.Regions)
	}
}

func TestBucketCreatedBy(t *testing.T) {
	for name, want := range map[string]string{
		"cdk-hnb659fds-assets-123456789012-eu-west-1": ManagedByCDK,
		"cf-templates-1a2b3c-eu-west-1":               ManagedByCloudFormation,
		"elasticbeanstalk-eu-west-1-123456789012":     ManagedByElasticBeanstalk,
		"acme-logs":          "",
		"cdk-something-else": "",
	} {
		if got := bucketCreatedBy(name); got != want {
			t.Errorf("bucketCreatedBy(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestS3TagRetryAfterStall(t *testing.T) {
	old := tagAttemptTimeout
	tagAttemptTimeout = 100 * time.Millisecond
	t.Cleanup(func() { tagAttemptTimeout = old })
	f := &fakeAWS{
		s3Pages:         map[string][]string{"eu-west-1": {bucketXML("slow", "eu-west-1")}},
		s3Tags:          map[string]map[string]string{"slow": {"team": "data"}},
		s3TagStallFirst: map[string]bool{"slow": true},
	}
	a := newTestAWS(t, f)
	pathStyle(a)
	inv, err := a.Discover(context.Background(), []string{"eu-west-1"}, TypeS3Bucket)
	if err != nil {
		t.Fatal(err)
	}
	b := inv.Instances()[0]
	if b.TagsUnavailable || b.Tags["team"] != "data" {
		t.Errorf("a stalled first attempt must be retried: %+v", b)
	}
	if n := f.count("GetBucketTagging/slow"); n != 2 {
		t.Errorf("tag calls = %d, want 2", n)
	}
}

// refusePathStyle fails every GetBucketTagging call, as a bucket that does
// not accept path-style requests would.
type refusePathStyle struct{ *s3.Client }

func (refusePathStyle) GetBucketTagging(context.Context, *s3.GetBucketTaggingInput, ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "PermanentRedirect", Message: "use the bucket endpoint"}
}

func TestS3TagsFallBackToVirtualHosted(t *testing.T) {
	f := &fakeAWS{
		s3Pages: map[string][]string{"eu-west-1": {bucketXML("picky", "eu-west-1") + bucketXML("untagged", "eu-west-1")}},
		s3Tags:  map[string]map[string]string{"picky": {"team": "data"}},
	}
	a := newTestAWS(t, f)
	pathStyle(a)
	virtual := a.newS3
	a.newS3PathStyle = func(c aws.Config, region string) s3API {
		return refusePathStyle{virtual(c, region).(*s3.Client)}
	}
	inv, err := a.Discover(context.Background(), []string{"eu-west-1"}, TypeS3Bucket)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Instance{}
	for _, b := range inv.Instances() {
		by[b.Identifier] = b
	}
	if by["picky"].Tags["team"] != "data" || by["picky"].TagsUnavailable {
		t.Errorf("picky: %+v", by["picky"])
	}
	if by["untagged"].TagsUnavailable || len(by["untagged"].Tags) != 0 {
		t.Errorf("untagged: %+v", by["untagged"])
	}
}

func TestS3LaterPageFailure(t *testing.T) {
	f := &fakeAWS{s3Pages: map[string][]string{"eu-west-1": {bucketXML("a-bucket", "eu-west-1"), bucketXML("b-bucket", "eu-west-1")}},
		failPage: map[string]int{"s3/eu-west-1": 1}}
	a := newTestAWS(t, f)
	pathStyle(a)
	inv, err := a.Discover(context.Background(), []string{"eu-west-1"}, TypeS3Bucket)
	if err != nil {
		t.Fatal(err)
	}
	if r := inv.Regions[0]; !r.Failed || len(r.Instances) != 0 {
		t.Errorf("a later-page failure must fail the region with no partial list: %+v", r)
	}
}

func TestS3TagRetriesAreBounded(t *testing.T) {
	old := tagAttemptTimeout
	tagAttemptTimeout = 100 * time.Millisecond
	t.Cleanup(func() { tagAttemptTimeout = old })
	f := &fakeAWS{s3Pages: map[string][]string{"eu-west-1": {bucketXML("stuck", "eu-west-1")}},
		s3Tags: map[string]map[string]string{"stuck": {"a": "b"}}, s3TagStallAlways: map[string]bool{"stuck": true}}
	a := newTestAWS(t, f)
	pathStyle(a)
	start := time.Now()
	inv, err := a.Discover(context.Background(), []string{"eu-west-1"}, TypeS3Bucket)
	if err != nil {
		t.Fatal(err)
	}
	if b := inv.Instances()[0]; !b.TagsUnavailable {
		t.Errorf("exhausted retries must be a tag warning: %+v", b)
	}
	if n := f.count("GetBucketTagging/stuck"); n != 2*tagAttempts {
		t.Errorf("tag calls = %d, want %d (attempts per addressing style x 2)", n, 2*tagAttempts)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %s", d)
	}
}

func TestS3TagConcurrencyIsBounded(t *testing.T) {
	old := tagWorkers
	tagWorkers = 4
	t.Cleanup(func() { tagWorkers = old })
	var page strings.Builder
	for i := 0; i < 30; i++ {
		page.WriteString(bucketXML(fmt.Sprintf("bucket-%02d", i), "eu-west-1"))
	}
	f := &fakeAWS{s3Pages: map[string][]string{"eu-west-1": {page.String()}}, slow: 20 * time.Millisecond}
	a := newTestAWS(t, f)
	pathStyle(a)
	inv, err := a.Discover(context.Background(), []string{"eu-west-1"}, TypeS3Bucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Instances()) != 30 {
		t.Fatalf("buckets = %d", len(inv.Instances()))
	}
	if f.maxSeen > 4 {
		t.Errorf("max concurrent S3 requests = %d, want <= 4", f.maxSeen)
	}
}

func TestS3CancellationStopsTagging(t *testing.T) {
	old := tagWorkers
	tagWorkers = 1
	t.Cleanup(func() { tagWorkers = old })
	var page strings.Builder
	for i := 0; i < 20; i++ {
		page.WriteString(bucketXML(fmt.Sprintf("bucket-%02d", i), "eu-west-1"))
	}
	f := &fakeAWS{s3Pages: map[string][]string{"eu-west-1": {page.String()}}, slow: 50 * time.Millisecond}
	a := newTestAWS(t, f)
	pathStyle(a)
	a.regionTimeout = 300 * time.Millisecond // listing (~50ms) succeeds, tagging is cut short
	inv, err := a.Discover(context.Background(), []string{"eu-west-1"}, TypeS3Bucket)
	if err != nil {
		t.Fatal(err)
	}
	unavailable := 0
	for _, b := range inv.Instances() {
		if b.TagsUnavailable {
			unavailable++
		}
	}
	tagCalls := f.countPrefix("GetBucketTagging/")
	if unavailable == 0 || tagCalls >= 20 {
		t.Errorf("after the deadline no new buckets should be tagged: %d tag calls, %d marked unavailable", tagCalls, unavailable)
	}
}

// count and countPrefix read the call counters under the lock: handlers for
// requests the client already gave up on can still be running.
func (f *fakeAWS) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *fakeAWS) countPrefix(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k, v := range f.calls {
		if strings.HasPrefix(k, prefix) {
			n += v
		}
	}
	return n
}

func (f *fakeAWS) serveLambda(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.inflight++
	if f.inflight > f.maxSeen {
		f.maxSeen = f.inflight
	}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inflight--; f.mu.Unlock() }()
	if f.slow > 0 {
		time.Sleep(f.slow)
	}
	region := regionFromAuth(r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")
	fail := func(code string) {
		w.Header().Set("X-Amzn-Errortype", code)
		w.WriteHeader(403)
		_, _ = fmt.Fprint(w, `{"Message":"OOS-SECRET-MARKER"}`)
	}
	if arn, ok := strings.CutPrefix(r.URL.Path, "/2017-03-31/tags/"); ok {
		f.mu.Lock()
		f.calls["ListTags/"+arn]++
		f.mu.Unlock()
		if f.lambdaTagFail[arn] {
			fail("AccessDeniedException")
			return
		}
		tags, _ := json.Marshal(f.lambdaTags[arn])
		_, _ = fmt.Fprintf(w, `{"Tags":%s}`, tags)
		return
	}
	f.mu.Lock()
	f.calls["ListFunctions/"+region]++
	f.lambdaQueries = append(f.lambdaQueries, r.URL.RawQuery)
	f.mu.Unlock()
	page := 0
	if m := r.URL.Query().Get("Marker"); m != "" {
		_, _ = fmt.Sscanf(m, "page-%d", &page)
	}
	if fp, ok := f.failPage["lambda/"+region]; ok && fp == page {
		fail(f.failCode)
		return
	}
	pages := f.lambdaPages[region]
	body := ""
	if page < len(pages) {
		body = pages[page]
	}
	marker := "null"
	if page+1 < len(pages) {
		marker = fmt.Sprintf(`"page-%d"`, page+1)
	}
	_, _ = fmt.Fprintf(w, `{"Functions":[%s],"NextMarker":%s}`, body, marker)
}

func fnARN(name string) string {
	return "arn:aws:lambda:eu-west-1:" + account + ":function:" + name
}

// fnJSON is a ListFunctions entry. Every field oos must not keep holds the
// secret marker.
func fnJSON(name string) string {
	return fmt.Sprintf(`{"FunctionName":%q,"FunctionArn":%q,"Runtime":"python3.13","PackageType":"Zip","Architectures":["arm64"],
"LastModified":"2025-05-01T09:00:00.000+0100","Handler":"OOS-SECRET-MARKER","Description":"OOS-SECRET-MARKER","Role":"arn:aws:iam::123456789012:role/OOS-SECRET-MARKER",
"Environment":{"Variables":{"API_KEY":"OOS-SECRET-MARKER"}}}`, name, fnARN(name))
}

// fastLambda turns off the pacing of tag calls for one test.
func fastLambda(t *testing.T) {
	t.Helper()
	old := lambdaTagInterval
	lambdaTagInterval = 0
	t.Cleanup(func() { lambdaTagInterval = old })
}

func TestLambdaDiscovery(t *testing.T) {
	fastLambda(t)
	image := fmt.Sprintf(`{"FunctionName":"job","FunctionArn":%q,"PackageType":"Image","Architectures":["x86_64"],"LastModified":"not a date"}`, fnARN("job"))
	f := &fakeAWS{
		lambdaPages: map[string][]string{"eu-west-1": {
			fnJSON("orders") + "," + fnJSON("stack-fn"),
			image + "," + fnJSON("aws-controltower-Forwarder") + "," + fnJSON("no-tag-access"),
		}},
		lambdaTags: map[string]map[string]string{
			fnARN("orders"):   {"team": "data"},
			fnARN("stack-fn"): {"aws:cloudformation:stack-name": "app"},
		},
		lambdaTagFail: map[string]bool{fnARN("no-tag-access"): true},
	}
	inv, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"}, TypeLambdaFunction)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Complete() || len(inv.Regions) != 1 || inv.Regions[0].Type != TypeLambdaFunction {
		t.Fatalf("%+v", inv.Regions)
	}
	by := map[string]Instance{}
	for _, fn := range inv.Instances() {
		by[fn.Identifier] = fn
	}
	if len(by) != 5 {
		t.Fatalf("functions = %d, want 5 (two pages)", len(by))
	}
	o := by["orders"]
	if o.Type != TypeLambdaFunction || o.Kind != KindLambda || o.ARN != fnARN("orders") || o.Region != "eu-west-1" || o.AccountID != account ||
		o.Runtime != "python3.13" || o.PackageType != "Zip" || o.Architecture != "arm64" ||
		o.LastModified != "2025-05-01T08:00:00Z" || o.Tags["team"] != "data" || o.TagsUnavailable || o.ManagedBy != "" || o.NotCovered != "" {
		t.Errorf("orders: %+v", o)
	}
	if j := by["job"]; j.Runtime != "" || j.PackageType != "Image" || j.Architecture != "x86_64" || j.LastModified != "" {
		t.Errorf("container image: %+v", j)
	}
	if by["stack-fn"].ManagedBy != ManagedByCloudFormation {
		t.Errorf("stack-fn: %+v", by["stack-fn"])
	}
	if c := by["aws-controltower-Forwarder"]; c.LikelyCreatedBy != ManagedByControlTower || c.ManagedBy != "" {
		t.Errorf("control tower: %+v", c)
	}
	if !by["no-tag-access"].TagsUnavailable {
		t.Errorf("tag failure must be a warning: %+v", by["no-tag-access"])
	}
	if dump := fmt.Sprintf("%+v %#v", inv, inv); strings.Contains(dump, "OOS-SECRET-MARKER") {
		t.Errorf("a field outside the allowlist was kept: %s", dump)
	}
	for _, q := range f.lambdaQueries {
		if strings.Contains(q, "FunctionVersion") {
			t.Errorf("published versions must not be requested: %q", q)
		}
	}
	if f.calls["ListFunctions/eu-west-1"] != 2 {
		t.Errorf("ListFunctions calls = %d, want 2", f.calls["ListFunctions/eu-west-1"])
	}
}

func TestLambdaFailureOnlyFailsThatType(t *testing.T) {
	fastLambda(t)
	f := &fakeAWS{pages: map[string][]string{"eu-west-1": {db("orders-db", "db-ORDERS", `<TagList></TagList>`)}},
		failPage: map[string]int{"lambda/eu-west-1": 0}, failCode: "AccessDeniedException"}
	inv, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"}, TypeDBInstance, TypeLambdaFunction)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Regions[0].Failed || !inv.Regions[1].Failed || inv.Regions[1].Error != ErrAccessDenied {
		t.Errorf("%+v", inv.Regions)
	}
}

func TestLambdaLaterPageFailure(t *testing.T) {
	fastLambda(t)
	f := &fakeAWS{lambdaPages: map[string][]string{"eu-west-1": {fnJSON("a"), fnJSON("b")}},
		failPage: map[string]int{"lambda/eu-west-1": 1}, failCode: "TooManyRequestsException"}
	a := newTestAWS(t, f)
	inv, err := a.Discover(context.Background(), []string{"eu-west-1"}, TypeLambdaFunction)
	if err != nil {
		t.Fatal(err)
	}
	if r := inv.Regions[0]; !r.Failed || r.Error != ErrThrottled || len(r.Instances) != 0 {
		t.Errorf("a later-page failure must fail the region with no partial list: %+v", r)
	}
}

func fnPage(n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fnJSON(fmt.Sprintf("fn-%02d", i))
	}
	return strings.Join(names, ",")
}

func TestLambdaTagConcurrencyIsBounded(t *testing.T) {
	fastLambda(t)
	old := lambdaTagWorkers
	lambdaTagWorkers = 3
	t.Cleanup(func() { lambdaTagWorkers = old })
	f := &fakeAWS{lambdaPages: map[string][]string{"eu-west-1": {fnPage(20)}}, slow: 20 * time.Millisecond}
	inv, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"}, TypeLambdaFunction)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Instances()) != 20 {
		t.Fatalf("functions = %d", len(inv.Instances()))
	}
	if f.maxSeen > 3 {
		t.Errorf("max concurrent Lambda requests = %d, want <= 3", f.maxSeen)
	}
}

func TestLambdaTagCallsArePaced(t *testing.T) {
	old := lambdaTagInterval
	lambdaTagInterval = 30 * time.Millisecond
	t.Cleanup(func() { lambdaTagInterval = old })
	f := &fakeAWS{lambdaPages: map[string][]string{"eu-west-1": {fnPage(5)}}}
	start := time.Now()
	if _, err := newTestAWS(t, f).Discover(context.Background(), []string{"eu-west-1"}, TypeLambdaFunction); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 120*time.Millisecond {
		t.Errorf("5 tag calls took %s, want at least 120ms", d)
	}
}

func TestLambdaTimeoutStopsTagging(t *testing.T) {
	old := lambdaTagInterval
	lambdaTagInterval = 50 * time.Millisecond
	t.Cleanup(func() { lambdaTagInterval = old })
	f := &fakeAWS{lambdaPages: map[string][]string{"eu-west-1": {fnPage(20)}}}
	a := newTestAWS(t, f)
	a.regionTimeout = 200 * time.Millisecond // listing succeeds, tagging is cut short
	inv, err := a.Discover(context.Background(), []string{"eu-west-1"}, TypeLambdaFunction)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Regions[0].Failed || len(inv.Instances()) != 20 {
		t.Fatalf("the listing was complete, so the region must not fail: %+v", inv.Regions[0])
	}
	unavailable := 0
	for _, fn := range inv.Instances() {
		if fn.TagsUnavailable {
			unavailable++
		}
	}
	if tagCalls := f.countPrefix("ListTags/"); unavailable == 0 || tagCalls >= 20 {
		t.Errorf("after the deadline no new calls should start: %d tag calls, %d marked unavailable", tagCalls, unavailable)
	}
}
