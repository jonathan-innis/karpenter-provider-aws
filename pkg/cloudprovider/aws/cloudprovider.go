/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/transport"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	k8sClient "sigs.k8s.io/controller-runtime/pkg/client"

	awsv1alpha1 "github.com/aws/karpenter/pkg/apis/awsnodetemplate/v1alpha1"
	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/amifamily"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/utils/functional"
	"github.com/aws/karpenter/pkg/utils/injection"
	"github.com/aws/karpenter/pkg/utils/project"
)

const (
	// CacheTTL restricts QPS to AWS APIs to this interval for verifying setup
	// resources. This value represents the maximum eventual consistency between
	// AWS actual state and the controller's ability to provision those
	// resources. Cache hits enable faster provisioning and reduced API load on
	// AWS APIs, which can have a serious impact on performance and scalability.
	// DO NOT CHANGE THIS VALUE WITHOUT DUE CONSIDERATION
	CacheTTL = 60 * time.Second
	// CacheCleanupInterval triggers cache cleanup (lazy eviction) at this interval.
	CacheCleanupInterval = 10 * time.Minute
	// MaxInstanceTypes defines the number of instance type options to pass to CreateFleet
	MaxInstanceTypes = 20
)

func init() {
	v1alpha5.NormalizedLabels = lo.Assign(v1alpha5.NormalizedLabels, map[string]string{"topology.ebs.csi.aws.com/zone": v1.LabelTopologyZone})
}

var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

type CloudProvider struct {
	instanceTypeProvider *InstanceTypeProvider
	instanceProvider     *InstanceProvider
	kubeClient           k8sClient.Client
	sqsProvider          *SQSProvider
}

func NewCloudProvider(ctx context.Context, options cloudprovider.Options) *CloudProvider {
	// if performing validation only, then only the Validate()/Default() methods will be called which
	// don't require any other setup
	if options.WebhookOnly {
		cp := &CloudProvider{}
		v1alpha5.ValidateHook = cp.Validate
		v1alpha5.DefaultHook = cp.Default
		return cp
	}

	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named("aws"))
	sess := withUserAgent(session.Must(session.NewSession(
		request.WithRetryer(
			&aws.Config{STSRegionalEndpoint: endpoints.RegionalSTSEndpoint},
			client.DefaultRetryer{NumMaxRetries: client.DefaultRetryerMaxNumRetries},
		),
	)))
	if *sess.Config.Region == "" {
		logging.FromContext(ctx).Debug("AWS region not configured, asking EC2 Instance Metadata Service")
		*sess.Config.Region = getRegionFromIMDS(sess)
	}
	logging.FromContext(ctx).Debugf("Using AWS region %s", *sess.Config.Region)

	ec2api := ec2.New(sess)
	if err := checkEC2Connectivity(ec2api); err != nil {
		logging.FromContext(ctx).Errorf("Checking EC2 API connectivity, %s", err)
	}
	sqsapi := sqs.New(sess)
	subnetProvider := NewSubnetProvider(ec2api)
	instanceTypeProvider := NewInstanceTypeProvider(ctx, sess, options, ec2api, subnetProvider)

	// TODO: Change this queue url value to a useful value
	sqsProvider := NewSQSProvider(sqsapi, "test-stack-Queue-VimlxX8fIySZ")
	cloudprovider := &CloudProvider{
		instanceTypeProvider: instanceTypeProvider,
		instanceProvider: NewInstanceProvider(ctx, ec2api, instanceTypeProvider, subnetProvider,
			NewLaunchTemplateProvider(
				ctx,
				ec2api,
				options.ClientSet,
				amifamily.New(ctx, ssm.New(sess), ec2api, cache.New(CacheTTL, CacheCleanupInterval), cache.New(CacheTTL, CacheCleanupInterval), options.KubeClient),
				NewSecurityGroupProvider(ec2api),
				getCABundle(ctx),
				options.StartAsync,
			),
		),
		sqsProvider: sqsProvider,
		kubeClient:  options.KubeClient,
	}
	v1alpha5.ValidateHook = cloudprovider.Validate
	v1alpha5.DefaultHook = cloudprovider.Default

	return cloudprovider
}

// checkEC2Connectivity makes a dry-run call to DescribeInstanceTypes.  If it fails, we provide an early indicator that we
// are having issues connecting to the EC2 API.
func checkEC2Connectivity(api *ec2.EC2) error {
	_, err := api.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{DryRun: aws.Bool(true)})
	var aerr awserr.Error
	if errors.As(err, &aerr) && aerr.Code() == "DryRunOperation" {
		return nil
	}
	return err
}

// Create a node given the constraints.
func (c *CloudProvider) Create(ctx context.Context, nodeRequest *cloudprovider.NodeRequest) (*v1.Node, error) {
	aws, err := c.getProvider(ctx, nodeRequest.Template.Provider, nodeRequest.Template.ProviderRef)
	if err != nil {
		return nil, err
	}
	return c.instanceProvider.Create(ctx, aws, nodeRequest)
}

func (c *CloudProvider) LivenessProbe(req *http.Request) error {
	if err := c.instanceTypeProvider.LivenessProbe(req); err != nil {
		return err
	}
	return nil
}

// GetInstanceTypes returns all available InstanceTypes
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, provisioner *v1alpha5.Provisioner) ([]cloudprovider.InstanceType, error) {
	aws, err := c.getProvider(ctx, provisioner.Spec.Provider, provisioner.Spec.ProviderRef)
	if err != nil {
		return nil, err
	}
	instanceTypes, err := c.instanceTypeProvider.Get(ctx, aws, provisioner.Spec.KubeletConfiguration)
	if err != nil {
		return nil, err
	}

	// if the provisioner is not supplying a list of instance types or families, perform some filtering to get instance
	// types that are suitable for general workloads
	if c.useOpinionatedInstanceFilter(provisioner.Spec.Requirements...) {
		instanceTypes = lo.Filter(instanceTypes, func(it cloudprovider.InstanceType, _ int) bool {
			cit, ok := it.(*InstanceType)
			if !ok {
				return true
			}

			// c3, m3 and r3 aren't current generation but are fine for general workloads
			if functional.HasAnyPrefix(*cit.InstanceType, "c3", "m3", "r3") {
				return true
			}

			// filter out all non-current generation
			if cit.CurrentGeneration != nil && !*cit.CurrentGeneration {
				return false
			}

			// t2 is current generation but has different bursting behavior and u- isn't widely available
			if functional.HasAnyPrefix(*cit.InstanceType, "t2", "u-") {
				return false
			}
			return true
		})
	}
	return instanceTypes, nil
}

func (c *CloudProvider) Delete(ctx context.Context, node *v1.Node) error {
	return c.instanceProvider.Terminate(ctx, node)
}

// Validate the provisioner
func (*CloudProvider) Validate(ctx context.Context, provisioner *v1alpha5.Provisioner) *apis.FieldError {
	// The receiver is intentionally omitted here as when used by the webhook, Validate/Default are the only methods
	// called and we don't fully initialize the CloudProvider to prevent some network calls to EC2/Pricing.
	if provisioner.Spec.Provider == nil {
		return nil
	}
	provider, err := v1alpha1.Deserialize(provisioner.Spec.Provider)
	if err != nil {
		return apis.ErrGeneric(err.Error())
	}
	return provider.Validate()
}

func (c *CloudProvider) SQSProvider() *SQSProvider {
	return c.sqsProvider
}

// Default the provisioner
func (*CloudProvider) Default(ctx context.Context, provisioner *v1alpha5.Provisioner) {
	defaultLabels(provisioner)
}

func defaultLabels(provisioner *v1alpha5.Provisioner) {
	for key, value := range map[string]string{
		v1alpha5.LabelCapacityType: ec2.DefaultTargetCapacityTypeOnDemand,
		v1.LabelArchStable:         v1alpha5.ArchitectureAmd64,
	} {
		hasLabel := false
		if _, ok := provisioner.Spec.Labels[key]; ok {
			hasLabel = true
		}
		for _, requirement := range provisioner.Spec.Requirements {
			if requirement.Key == key {
				hasLabel = true
			}
		}
		if !hasLabel {
			provisioner.Spec.Requirements = append(provisioner.Spec.Requirements, v1.NodeSelectorRequirement{
				Key: key, Operator: v1.NodeSelectorOpIn, Values: []string{value},
			})
		}
	}
}

// Name returns the CloudProvider implementation name.
func (c *CloudProvider) Name() string {
	return "aws"
}

// get the current region from EC2 IMDS
func getRegionFromIMDS(sess *session.Session) string {
	region, err := ec2metadata.New(sess).Region()
	if err != nil {
		panic(fmt.Sprintf("Failed to call the metadata server's region API, %s", err))
	}
	return region
}

// withUserAgent adds a karpenter specific user-agent string to AWS session
func withUserAgent(sess *session.Session) *session.Session {
	userAgent := fmt.Sprintf("karpenter.sh-%s", project.Version)
	sess.Handlers.Build.PushBack(request.MakeAddToUserAgentFreeFormHandler(userAgent))
	return sess
}

func getCABundle(ctx context.Context) *string {
	// Discover CA Bundle from the REST client. We could alternatively
	// have used the simpler client-go InClusterConfig() method.
	// However, that only works when Karpenter is running as a Pod
	// within the same cluster it's managing.
	restConfig := injection.GetConfig(ctx)
	if restConfig == nil {
		return nil
	}
	transportConfig, err := restConfig.TransportConfig()
	if err != nil {
		logging.FromContext(ctx).Fatalf("Unable to discover caBundle, loading transport config, %v", err)
		return nil
	}
	_, err = transport.TLSConfigFor(transportConfig) // fills in CAData!
	if err != nil {
		logging.FromContext(ctx).Fatalf("Unable to discover caBundle, loading TLS config, %v", err)
		return nil
	}
	logging.FromContext(ctx).Debugf("Discovered caBundle, length %d", len(transportConfig.TLS.CAData))
	return ptr.String(base64.StdEncoding.EncodeToString(transportConfig.TLS.CAData))
}

func (c *CloudProvider) getProvider(ctx context.Context, provider *runtime.RawExtension, providerRef *v1alpha5.ProviderRef) (*v1alpha1.AWS, error) {
	if providerRef != nil {
		var ant awsv1alpha1.AWSNodeTemplate
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: providerRef.Name}, &ant); err != nil {
			return nil, fmt.Errorf("getting providerRef, %w", err)
		}
		return &ant.Spec.AWS, nil
	}
	aws, err := v1alpha1.Deserialize(provider)
	if err != nil {
		return nil, err
	}
	return aws, nil
}

func (c *CloudProvider) useOpinionatedInstanceFilter(provisionerRequirements ...v1.NodeSelectorRequirement) bool {
	var instanceRequirements []v1.NodeSelectorRequirement
	requirementKeys := []string{v1.LabelInstanceTypeStable, v1alpha1.LabelInstanceFamily, v1alpha1.LabelInstanceCategory, v1alpha1.LabelInstanceGeneration}

	for _, r := range provisionerRequirements {
		if lo.Contains(requirementKeys, r.Key) {
			instanceRequirements = append(instanceRequirements, r)
		}
	}
	// no provisioner instance type filtering, so use our opinionated list
	if len(instanceRequirements) == 0 {
		return true
	}

	for _, req := range instanceRequirements {
		switch req.Operator {
		case v1.NodeSelectorOpIn, v1.NodeSelectorOpExists, v1.NodeSelectorOpDoesNotExist:
			// v1.NodeSelectorOpIn: provisioner supplies its own list of instance types/families, so use that instead of filtering
			// v1.NodeSelectorOpExists: provisioner explicitly is asking for no filtering
			// v1.NodeSelectorOpDoesNotExist: this shouldn't match any instance type at provisioning time, but avoid filtering anyway
			return false
		case v1.NodeSelectorOpNotIn, v1.NodeSelectorOpGt, v1.NodeSelectorOpLt:
			// provisioner further restricts instance types/families, so we can possibly use our list and it will
			// be filtered more
		}
	}

	// provisioner requirements haven't prevented us from filtering
	return true
}
