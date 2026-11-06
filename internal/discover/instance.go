// Package discover lists AWS resources: RDS DB instances, Elastic Load
// Balancing v2 load balancers and S3 general purpose buckets.
package discover

import (
	"regexp"
	"strings"
	"time"
)

// Terraform resource types oos supports. Every discovered resource carries
// one, and matching only compares resources of the same type.
const (
	TypeDBInstance   = "aws_db_instance"
	TypeLoadBalancer = "aws_lb" // also written as aws_alb in older code
	TypeS3Bucket     = "aws_s3_bucket"
)

// Kinds are short labels for reports.
const (
	KindRDS  = "rds"
	KindALB  = "alb"
	KindNLB  = "nlb"
	KindGWLB = "gwlb"
	KindS3   = "s3"
)

// Instance is one discovered AWS resource. The name is historical: it holds
// RDS DB instances and load balancers alike; Type says which.
type Instance struct {
	Type string // Terraform resource type; empty means aws_db_instance
	Kind string // report label: rds, alb, nlb, gwlb

	Partition string
	AccountID string
	Region    string

	ARN        string
	Identifier string // DB identifier, or load balancer name
	ResourceID string // DbiResourceId, "db-..." (RDS only)

	// RDS details.
	Engine        string
	EngineVersion string
	Class         string
	ClusterID     string // DBClusterIdentifier, non-empty for cluster members

	// Load balancer details.
	Scheme string // internet-facing, internal
	VPCID  string

	Status string // DBInstanceStatus, or load balancer state
	Tags   map[string]string
	// TagsUnavailable is set when tags could not be read. It is a warning
	// only: the resource is still matched normally.
	TagsUnavailable bool
	Created         time.Time

	// NotCovered is non-empty when the resource is outside what oos checks,
	// and says why. Set by Classify.
	NotCovered string
	// ManagedBy names another tool that, judging by the tags, manages the
	// resource (cloudformation, elastic_beanstalk, kubernetes). Set by
	// Classify. It is a hint for the report, not proof.
	ManagedBy string
	// LikelyCreatedBy names a tool whose naming convention the resource
	// follows (for S3: cdk, cloudformation, elastic_beanstalk). Weaker than
	// ManagedBy: a name says nothing about who manages it now.
	LikelyCreatedBy string
}

// TerraformType returns Type, defaulting to aws_db_instance.
func (i *Instance) TerraformType() string {
	if i.Type == "" {
		return TypeDBInstance
	}
	return i.Type
}

// Reasons a resource is not covered.
const (
	NotCoveredClusterMember = "member of an Aurora or Multi-AZ DB cluster"
	NotCoveredDocumentDB    = "DocumentDB instance"
	NotCoveredNeptune       = "Neptune instance"
	NotCoveredGatewayLB     = "Gateway Load Balancer"
)

// Tools that can be inferred from tags.
const (
	ManagedByCloudFormation   = "cloudformation"
	ManagedByElasticBeanstalk = "elastic_beanstalk"
	ManagedByKubernetes       = "kubernetes"
	ManagedByCDK              = "cdk"
)

// Classify sets NotCovered and ManagedBy. Ordinary Multi-AZ DB instances
// stay in scope.
func Classify(inst *Instance) {
	inst.NotCovered = ""
	switch inst.TerraformType() {
	case TypeDBInstance:
		if inst.Kind == "" {
			inst.Kind = KindRDS
		}
		switch {
		case inst.Engine == "docdb":
			inst.NotCovered = NotCoveredDocumentDB
		case inst.Engine == "neptune":
			inst.NotCovered = NotCoveredNeptune
		case inst.ClusterID != "":
			inst.NotCovered = NotCoveredClusterMember
		}
	case TypeLoadBalancer:
		if inst.Kind == KindGWLB {
			inst.NotCovered = NotCoveredGatewayLB
		}
	case TypeS3Bucket:
		inst.Kind = KindS3
	}
	inst.ManagedBy = managedBy(inst.Tags)
	inst.LikelyCreatedBy = ""
	if inst.ManagedBy == "" && inst.TerraformType() == TypeS3Bucket {
		inst.LikelyCreatedBy = bucketCreatedBy(inst.Identifier)
	}
}

// cdkAssetsBucketRE matches CDK bootstrap asset buckets, for example
// cdk-hnb659fds-assets-123456789012-eu-west-1.
var cdkAssetsBucketRE = regexp.MustCompile(`^cdk-[a-z0-9]+-assets-[0-9]{12}-[a-z0-9-]+$`)

// bucketCreatedBy recognises buckets that tools create with well-known
// names. It is a hint for the report, not proof.
func bucketCreatedBy(name string) string {
	switch {
	case cdkAssetsBucketRE.MatchString(name):
		return ManagedByCDK
	case strings.HasPrefix(name, "cf-templates-"):
		return ManagedByCloudFormation
	case strings.HasPrefix(name, "elasticbeanstalk-"):
		return ManagedByElasticBeanstalk
	}
	return ""
}

// managedBy infers another managing tool from well-known tags. Kubernetes
// wins over CloudFormation: the AWS Load Balancer Controller can run in a
// cluster that CloudFormation created.
func managedBy(tags map[string]string) string {
	has := func(keys ...string) bool {
		for _, k := range keys {
			if _, ok := tags[k]; ok {
				return true
			}
		}
		return false
	}
	switch {
	case has("elbv2.k8s.aws/cluster", "ingress.k8s.aws/stack", "service.k8s.aws/stack"):
		return ManagedByKubernetes
	case has("elasticbeanstalk:environment-name", "elasticbeanstalk:environment-id"):
		return ManagedByElasticBeanstalk
	case has("aws:cloudformation:stack-name", "aws:cloudformation:stack-id"):
		return ManagedByCloudFormation
	}
	for k := range tags {
		if strings.HasPrefix(k, "kubernetes.io/cluster/") {
			return ManagedByKubernetes
		}
	}
	return ""
}

// LBKind maps an ELBv2 load balancer type to a report label.
func LBKind(lbType string) string {
	switch lbType {
	case "application":
		return KindALB
	case "network":
		return KindNLB
	case "gateway":
		return KindGWLB
	}
	return lbType
}
