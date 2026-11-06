package discover

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// Errors that stop a scan before any report can be produced.
const (
	ErrNoCredentials   ErrorCategory = "no_credentials"
	ErrProfileNotFound ErrorCategory = "profile_not_found"
	ErrNoRegion        ErrorCategory = "no_region"
)

func init() {
	errorMessages[ErrNoCredentials] = "no AWS credentials were found (check --profile, or run 'aws sso login')"
	errorMessages[ErrProfileNotFound] = "the AWS profile does not exist in your AWS config"
	errorMessages[ErrNoRegion] = "no region is configured: pass --region or set one in your AWS profile"
}

// FatalError is a discovery failure that stops the scan (exit code 1).
type FatalError struct {
	Category ErrorCategory
	Step     string // what oos was doing, e.g. "identifying the AWS account"
}

func (e *FatalError) Error() string {
	return fmt.Sprintf("%s: %s", e.Step, e.Category.Message())
}

// stsAPI and rdsAPI are interfaces so tests can point the real SDK clients
// at a local HTTP server.
type stsAPI interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

type rdsAPI interface {
	rds.DescribeDBInstancesAPIClient
	ListTagsForResource(context.Context, *rds.ListTagsForResourceInput, ...func(*rds.Options)) (*rds.ListTagsForResourceOutput, error)
}

type elbAPI interface {
	elbv2.DescribeLoadBalancersAPIClient
	DescribeTags(context.Context, *elbv2.DescribeTagsInput, ...func(*elbv2.Options)) (*elbv2.DescribeTagsOutput, error)
}

// maxConcurrentRegions bounds how many regions are scanned at once.
const maxConcurrentRegions = 4

// AWS discovers resources with the AWS SDK.
type AWS struct {
	cfg             aws.Config
	newSTS          func(aws.Config) stsAPI
	newRDS          func(aws.Config, string) rdsAPI
	newELB          func(aws.Config, string) elbAPI
	regionTimeout   time.Duration
	identityTimeout time.Duration
	maxConcurrent   int
}

// NewAWS loads the shared AWS configuration (environment, ~/.aws/config,
// SSO profiles) for the given profile, or the default chain when empty.
func NewAWS(ctx context.Context, profile string) (*AWS, error) {
	var opts []func(*config.LoadOptions) error
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		var notExist config.SharedConfigProfileNotExistError
		if errors.As(err, &notExist) {
			return nil, &FatalError{Category: ErrProfileNotFound, Step: "loading AWS configuration"}
		}
		return nil, &FatalError{Category: ErrAPI, Step: "loading AWS configuration"}
	}
	return newAWS(cfg), nil
}

func newAWS(cfg aws.Config) *AWS {
	return &AWS{
		cfg:    cfg,
		newSTS: func(c aws.Config) stsAPI { return sts.NewFromConfig(c) },
		newRDS: func(c aws.Config, region string) rdsAPI {
			return rds.NewFromConfig(c, func(o *rds.Options) { o.Region = region })
		},
		newELB: func(c aws.Config, region string) elbAPI {
			return elbv2.NewFromConfig(c, func(o *elbv2.Options) { o.Region = region })
		},
		regionTimeout:   2 * time.Minute,
		identityTimeout: 30 * time.Second,
		maxConcurrent:   maxConcurrentRegions,
	}
}

// Config is the loaded AWS configuration, for other AWS clients (cost).
func (a *AWS) Config() aws.Config { return a.cfg }

// DefaultRegion is the region from the AWS configuration, if any.
func (a *AWS) DefaultRegion() string { return a.cfg.Region }

// Discover identifies the account and lists resources of the given Terraform
// types (default: aws_db_instance) in each region. A failure for one region
// and type is recorded in the inventory; an identity failure is a
// FatalError.
func (a *AWS) Discover(ctx context.Context, regions []string, types ...string) (*Inventory, error) {
	if len(types) == 0 {
		types = []string{TypeDBInstance}
	}
	if len(regions) == 0 {
		return nil, &FatalError{Category: ErrNoRegion, Step: "choosing regions"}
	}
	idCfg := a.cfg.Copy()
	if idCfg.Region == "" {
		idCfg.Region = regions[0]
	}
	idCtx, cancel := context.WithTimeout(ctx, a.identityTimeout)
	id, err := a.newSTS(idCfg).GetCallerIdentity(idCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		category := categorise(idCtx, err, true)
		cancel()
		return nil, &FatalError{Category: category, Step: "identifying the AWS account"}
	}
	cancel()
	partition := partitionOf(aws.ToString(id.Arn))
	account := aws.ToString(id.Account)
	if partition == "" || account == "" {
		return nil, &FatalError{Category: ErrAPI, Step: "identifying the AWS account"}
	}

	// One job per region and type, results in region-major order.
	inv := &Inventory{Partition: partition, AccountID: account, Regions: make([]RegionResult, len(regions)*len(types))}
	var wg sync.WaitGroup
	slots := make(chan struct{}, max(1, a.maxConcurrent))
	for ri, region := range regions {
		for ti, typ := range types {
			wg.Add(1)
			go func() {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				var rr RegionResult
				switch typ {
				case TypeLoadBalancer:
					rr = a.regionLB(ctx, partition, account, region)
				default:
					rr = a.region(ctx, partition, account, region)
				}
				rr.Type = typ
				inv.Regions[ri*len(types)+ti] = rr
			}()
		}
	}
	wg.Wait()
	return inv, nil
}

func (a *AWS) region(ctx context.Context, partition, account, region string) RegionResult {
	ctx, cancel := context.WithTimeout(ctx, a.regionTimeout)
	defer cancel()

	rr := RegionResult{Region: region}
	client := a.newRDS(a.cfg, region)
	p := rds.NewDescribeDBInstancesPaginator(client, &rds.DescribeDBInstancesInput{})
	var instances []Instance
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			// A failure on any page fails the region: a partial list is
			// never reported as complete.
			return RegionResult{Region: region, Failed: true, Error: categorise(ctx, err, false)}
		}
		for _, db := range page.DBInstances {
			inst := Instance{
				Type:          TypeDBInstance,
				Kind:          KindRDS,
				Partition:     partition,
				AccountID:     account,
				Region:        region,
				ARN:           aws.ToString(db.DBInstanceArn),
				Identifier:    aws.ToString(db.DBInstanceIdentifier),
				ResourceID:    aws.ToString(db.DbiResourceId),
				Engine:        aws.ToString(db.Engine),
				EngineVersion: aws.ToString(db.EngineVersion),
				Class:         aws.ToString(db.DBInstanceClass),
				Status:        aws.ToString(db.DBInstanceStatus),
				ClusterID:     aws.ToString(db.DBClusterIdentifier),
				Created:       aws.ToTime(db.InstanceCreateTime),
			}
			tagList := db.TagList
			if tagList == nil && inst.ARN != "" {
				// Older responses can omit TagList; fall back to a separate
				// call. A failure here is a warning, never a failed region.
				out, tagErr := client.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{ResourceName: db.DBInstanceArn})
				if tagErr != nil {
					inst.TagsUnavailable = true
				} else {
					tagList = out.TagList
				}
			}
			if len(tagList) > 0 {
				inst.Tags = make(map[string]string, len(tagList))
				for _, t := range tagList {
					inst.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
				}
			}
			Classify(&inst)
			instances = append(instances, inst)
		}
	}
	rr.Instances = instances
	return rr
}

// describeTagsBatch is the most ARNs DescribeTags accepts in one call.
const describeTagsBatch = 20

func (a *AWS) regionLB(ctx context.Context, partition, account, region string) RegionResult {
	ctx, cancel := context.WithTimeout(ctx, a.regionTimeout)
	defer cancel()

	client := a.newELB(a.cfg, region)
	p := elbv2.NewDescribeLoadBalancersPaginator(client, &elbv2.DescribeLoadBalancersInput{})
	var lbs []Instance
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			// A failure on any page fails the region for this type.
			return RegionResult{Region: region, Failed: true, Error: categorise(ctx, err, false)}
		}
		for _, lb := range page.LoadBalancers {
			inst := Instance{
				Type:       TypeLoadBalancer,
				Kind:       LBKind(string(lb.Type)),
				Partition:  partition,
				AccountID:  account,
				Region:     region,
				ARN:        aws.ToString(lb.LoadBalancerArn),
				Identifier: aws.ToString(lb.LoadBalancerName),
				Scheme:     string(lb.Scheme),
				VPCID:      aws.ToString(lb.VpcId),
				Created:    aws.ToTime(lb.CreatedTime),
			}
			if lb.State != nil {
				inst.Status = string(lb.State.Code)
			}
			lbs = append(lbs, inst)
		}
	}

	// Tags come from a separate call, at most 20 ARNs at a time. A failure
	// is a warning on the affected load balancers, never a failed region.
	for start := 0; start < len(lbs); start += describeTagsBatch {
		batch := lbs[start:min(start+describeTagsBatch, len(lbs))]
		arns := make([]string, 0, len(batch))
		for _, lb := range batch {
			arns = append(arns, lb.ARN)
		}
		out, err := client.DescribeTags(ctx, &elbv2.DescribeTagsInput{ResourceArns: arns})
		if err != nil {
			for i := range batch {
				batch[i].TagsUnavailable = true
			}
			continue
		}
		byARN := map[string]map[string]string{}
		for _, td := range out.TagDescriptions {
			m := map[string]string{}
			for _, t := range td.Tags {
				m[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			byARN[aws.ToString(td.ResourceArn)] = m
		}
		for i := range batch {
			if tags, ok := byARN[batch[i].ARN]; ok {
				if len(tags) > 0 {
					batch[i].Tags = tags
				}
			} else {
				batch[i].TagsUnavailable = true
			}
		}
	}
	for i := range lbs {
		Classify(&lbs[i])
	}
	return RegionResult{Region: region, Instances: lbs}
}

// categorise maps an SDK error to a fixed category. The error text itself is
// never kept.
func categorise(ctx context.Context, err error, identity bool) ErrorCategory {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation", "UnauthorizedAccess":
			return ErrAccessDenied
		case "Throttling", "ThrottlingException", "RequestLimitExceeded", "TooManyRequestsException":
			return ErrThrottled
		case "InvalidClientTokenId", "UnrecognizedClientException", "AuthFailure", "OptInRequired":
			if identity {
				return ErrNoCredentials
			}
			return ErrRegionDisabled
		case "ExpiredToken", "ExpiredTokenException":
			return ErrNoCredentials
		}
		return ErrAPI
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "credential") || strings.Contains(msg, "sso") {
		return ErrNoCredentials
	}
	return ErrAPI
}

// partitionOf returns the partition from an ARN such as
// arn:aws:iam::123456789012:user/name.
func partitionOf(arn string) string {
	parts := strings.SplitN(arn, ":", 3)
	if len(parts) < 3 || parts[0] != "arn" {
		return ""
	}
	return parts[1]
}
