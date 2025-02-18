//go:generate mockgen -destination ./mock/client.go . Client
package client

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/sirupsen/logrus"
	"k8s.io/utils/pointer"
)

// Client is an abstraction on the AWS SDK to enable easier mocking and manipulation of request data.
type Client interface {
	// GetRegion returns the AWS EC2 instance region. It can be discovered by using WithMetadataDiscovery opt, which
	// will dynamically get the region from the instance metadata endpoint. Or, it can be overriden by setting the
	// environment variable EKS_REGION.
	GetRegion(ctx context.Context) (*string, error)
	// GetAccountID returns the AWS EC2 instance account ID. It can be discovered by using WithMetadataDiscovery opt,
	// which will dynamically get the account ID from the instance metadata endpoint. Or, it can be overriden by setting
	// the environment variable EKS_ACCOUNT_ID.
	GetAccountID(ctx context.Context) (*string, error)
	// GetClusterName returns the AWS EKS cluster name. It can be discovered dynamically by using WithMetadataDiscovery
	// opt, which will dynamically get the cluster name by doing a combination of metadata and EC2 SDK calls. Or, it can
	// be overridden by setting the environment variable EKS_CLUSTER_NAME.
	GetClusterName(ctx context.Context) (*string, error)
	// GetInstancesByInstanceIDs returns a list of EC2 instances from the EC2 SDK by filtering on the instance IDs which
	// can be retrieved from node.spec.providerID.
	GetInstancesByInstanceIDs(ctx context.Context, instanceIDs []string) ([]*ec2.Instance, error)
}

// New creates and configures a new AWS Client instance.
func New(ctx context.Context, log logrus.FieldLogger, opts ...Opt) (Client, error) {
	c := &client{log: log}

	for _, opt := range opts {
		if err := opt(ctx, c); err != nil {
			return nil, err
		}
	}

	return c, nil
}

const (
	tagEKSK8sCluster         = "k8s.io/cluster/"
	tagEKSKubernetesCluster  = "kubernetes.io/cluster/"
	tagKOPSKubernetesCluster = "KubernetesCluster"
	owned                    = "owned"
)

var (
	eksClusterTags = []string{
		tagEKSK8sCluster,
		tagEKSKubernetesCluster,
	}
)

// Opt for configuring the AWS Client.
type Opt func(ctx context.Context, c *client) error

// WithEC2Client configures an EC2 SDK client. AWS region must be already discovered or set on an environment variable.
func WithEC2Client() func(ctx context.Context, c *client) error {
	return func(ctx context.Context, c *client) error {
		sess, err := session.NewSession(aws.
			NewConfig().
			WithRegion(*c.region).
			WithCredentialsChainVerboseErrors(true))
		if err != nil {
			return fmt.Errorf("creating aws sdk session: %w", err)
		}

		c.sess = sess
		c.ec2Client = ec2.New(sess)

		return nil
	}
}

// WithValidateCredentials validates the aws-sdk credentials chain.
func WithValidateCredentials() func(ctx context.Context, c *client) error {
	return func(ctx context.Context, c *client) error {
		if _, err := c.sess.Config.Credentials.Get(); err != nil {
			return fmt.Errorf("validating aws credentials: %w", err)
		}
		return nil
	}
}

// WithMetadata configures the discoverable EC2 instance metadata and EKS properties by setting static values instead
// of relying on the discovery mechanism.
func WithMetadata(accountID, region, clusterName string) func(ctx context.Context, c *client) error {
	return func(ctx context.Context, c *client) error {
		c.accountID = &accountID
		c.region = &region
		c.clusterName = &clusterName
		return nil
	}
}

// WithMetadataDiscovery configures the EC2 instance metadata client to enable dynamic discovery of those properties.
func WithMetadataDiscovery() func(ctx context.Context, c *client) error {
	return func(ctx context.Context, c *client) error {
		metaSess, err := session.NewSession()
		if err != nil {
			return fmt.Errorf("creating metadata session: %w", err)
		}

		c.metaClient = ec2metadata.New(metaSess)

		region, err := c.metaClient.RegionWithContext(ctx)
		if err != nil {
			return fmt.Errorf("getting instance region: %w", err)
		}

		c.region = &region

		return nil
	}
}

type client struct {
	log         logrus.FieldLogger
	sess        *session.Session
	metaClient  *ec2metadata.EC2Metadata
	ec2Client   *ec2.EC2
	region      *string
	accountID   *string
	clusterName *string
}

func (c *client) GetRegion(ctx context.Context) (*string, error) {
	if c.region != nil {
		return c.region, nil
	}

	region, err := c.metaClient.RegionWithContext(ctx)
	if err != nil {
		return nil, err
	}

	c.region = &region

	return c.region, nil
}

func (c *client) GetAccountID(ctx context.Context) (*string, error) {
	if c.accountID != nil {
		return c.accountID, nil
	}

	resp, err := c.metaClient.GetInstanceIdentityDocumentWithContext(ctx)
	if err != nil {
		return nil, err
	}

	c.accountID = &resp.AccountID

	return c.accountID, nil
}

func (c *client) GetClusterName(ctx context.Context) (*string, error) {
	if c.clusterName != nil {
		return c.clusterName, nil
	}

	instanceID, err := c.metaClient.GetMetadataWithContext(ctx, "instance-id")
	if err != nil {
		return nil, fmt.Errorf("getting instance id from metadata: %w", err)
	}

	resp, err := c.ec2Client.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []*string{pointer.StringPtr(instanceID)},
	})
	if err != nil {
		return nil, fmt.Errorf("describing instance_id=%s: %w", instanceID, err)
	}

	if len(resp.Reservations) == 0 {
		return nil, fmt.Errorf("no reservations found for instance_id=%s", instanceID)
	}

	if len(resp.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("no instances found for instance_id=%s", instanceID)
	}

	if len(resp.Reservations[0].Instances[0].Tags) == 0 {
		return nil, fmt.Errorf("no tags found for instance_id=%s", instanceID)
	}

	clusterName := getClusterName(resp.Reservations[0].Instances[0].Tags)
	if clusterName == "" {
		return nil, fmt.Errorf("discovering cluster name: instance cluster tags not found for instance_id=%s", instanceID)
	}

	c.clusterName = &clusterName

	return c.clusterName, nil
}

func getClusterName(tags []*ec2.Tag) string {
	for _, tag := range tags {
		if tag == nil || tag.Key == nil || tag.Value == nil {
			continue
		}
		for _, clusterTag := range eksClusterTags {
			if strings.HasPrefix(*tag.Key, clusterTag) && *tag.Value == owned {
				return strings.TrimPrefix(*tag.Key, clusterTag)
			}
		}
		if *tag.Key == tagKOPSKubernetesCluster {
			return *tag.Value
		}
	}
	return ""
}

func (c *client) GetInstancesByInstanceIDs(ctx context.Context, instanceIDs []string) ([]*ec2.Instance, error) {
	idsPtr := make([]*string, len(instanceIDs))
	for i := range instanceIDs {
		idsPtr[i] = &instanceIDs[i]
	}

	var instances []*ec2.Instance

	batchSize := 20
	for i := 0; i < len(idsPtr); i += batchSize {
		batch := idsPtr[i:int(math.Min(float64(i+batchSize), float64(len(idsPtr))))]

		req := &ec2.DescribeInstancesInput{
			Filters: []*ec2.Filter{
				{
					Name:   pointer.StringPtr("instance-id"),
					Values: batch,
				},
			},
		}

		resp, err := c.ec2Client.DescribeInstancesWithContext(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("describing instances: %w", err)
		}

		for _, reservation := range resp.Reservations {
			instances = append(instances, reservation.Instances...)
		}
	}

	return instances, nil
}
