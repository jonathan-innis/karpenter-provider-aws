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

package instanceprofile

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/iam/iamiface"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"

	corev1beta1 "github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter/pkg/apis/settings"
	"github.com/aws/karpenter/pkg/apis/v1beta1"
	awserrors "github.com/aws/karpenter/pkg/errors"
	"github.com/aws/karpenter/pkg/utils"
)

var (
	instanceStateFilter = &ec2.Filter{
		Name:   aws.String("instance-state-name"),
		Values: aws.StringSlice([]string{ec2.InstanceStateNamePending, ec2.InstanceStateNameRunning, ec2.InstanceStateNameStopping, ec2.InstanceStateNameStopped, ec2.InstanceStateNameShuttingDown}),
	}
)

type Provider struct {
	region string
	iamapi iamiface.IAMAPI
	ec2api ec2iface.EC2API
	cache  *cache.Cache
}

func NewProvider(region string, iamapi iamiface.IAMAPI, ec2api ec2iface.EC2API, cache *cache.Cache) *Provider {
	return &Provider{
		region: region,
		iamapi: iamapi,
		ec2api: ec2api,
		cache:  cache,
	}
}

func (p *Provider) Create(ctx context.Context, nodeClass *v1beta1.EC2NodeClass) (string, error) {
	tags := lo.Assign(nodeClass.Spec.Tags, map[string]string{
		fmt.Sprintf("kubernetes.io/cluster/%s", settings.FromContext(ctx).ClusterName): "owned",
		corev1beta1.ManagedByAnnotationKey:                                             settings.FromContext(ctx).ClusterName,
		v1beta1.LabelNodeClass:                                                         nodeClass.Name,
		v1.LabelTopologyRegion:                                                         p.region,
	})
	profileName := GetProfileName(ctx, nodeClass)

	// An instance profile exists for this NodeClass
	if _, ok := p.cache.Get(string(nodeClass.UID)); ok {
		return profileName, nil
	}
	// Validate if the instance profile exists and has the correct role assigned to it
	var instanceProfile *iam.InstanceProfile
	out, err := p.iamapi.GetInstanceProfileWithContext(ctx, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String(profileName)})
	if err != nil {
		if !awserrors.IsNotFound(err) {
			return "", fmt.Errorf("getting instance profile %q, %w", profileName, err)
		}
		o, err := p.iamapi.CreateInstanceProfileWithContext(ctx, &iam.CreateInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
			Tags:                lo.MapToSlice(tags, func(k, v string) *iam.Tag { return &iam.Tag{Key: aws.String(k), Value: aws.String(v)} }),
		})
		if err != nil {
			return "", fmt.Errorf("creating instance profile %q, %w", profileName, err)
		}
		instanceProfile = o.InstanceProfile
	} else {
		instanceProfile = out.InstanceProfile
	}
	if len(instanceProfile.Roles) == 1 {
		if aws.StringValue(instanceProfile.Roles[0].RoleName) == nodeClass.Spec.Role {
			return profileName, nil
		}
		if _, err = p.iamapi.RemoveRoleFromInstanceProfileWithContext(ctx, &iam.RemoveRoleFromInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
			RoleName:            instanceProfile.Roles[0].RoleName,
		}); err != nil {
			return "", fmt.Errorf("removing role %q for instance profile %q, %w", aws.StringValue(instanceProfile.Roles[0].RoleName), profileName, err)
		}
	}
	if _, err = p.iamapi.AddRoleToInstanceProfileWithContext(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
		RoleName:            aws.String(nodeClass.Spec.Role),
	}); err != nil {
		return "", fmt.Errorf("adding role %q to instance profile %q, %w", nodeClass.Spec.Role, profileName, err)
	}
	p.cache.SetDefault(string(nodeClass.UID), aws.StringValue(instanceProfile.Arn))
	return profileName, nil
}

func (p *Provider) AssociatedInstances(ctx context.Context, nodeClass *v1beta1.EC2NodeClass) ([]string, error) {
	profileName := GetProfileName(ctx, nodeClass)

	// We need to grab the instance profile ARN to filter out instances using it
	var arn string
	if v, ok := p.cache.Get(string(nodeClass.UID)); ok {
		arn = v.(string)
	} else {
		out, err := p.iamapi.GetInstanceProfileWithContext(ctx, &iam.GetInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
		})
		if err != nil {
			return nil, awserrors.IgnoreNotFound(fmt.Errorf("retrieving instance profile %q arn", profileName))
		}
		arn = aws.StringValue(out.InstanceProfile.Arn)
	}

	// Get all instances that are using our instance profile name and are not yet terminated
	var ids []string
	if err := p.ec2api.DescribeInstancesPagesWithContext(ctx, &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("iam-instance-profile.arn"),
				Values: aws.StringSlice([]string{arn}),
			},
			instanceStateFilter,
		},
	}, func(page *ec2.DescribeInstancesOutput, _ bool) bool {
		for _, res := range page.Reservations {
			ids = append(ids, lo.Map(res.Instances, func(i *ec2.Instance, _ int) string {
				return aws.StringValue(i.InstanceId)
			})...)
		}
		return true
	}); err != nil {
		return nil, fmt.Errorf("getting associated instances for instance profile %q, %w", profileName, err)
	}
	return ids, nil
}

func (p *Provider) Delete(ctx context.Context, nodeClass *v1beta1.EC2NodeClass) error {
	profileName := GetProfileName(ctx, nodeClass)
	out, err := p.iamapi.GetInstanceProfileWithContext(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
	})
	if err != nil {
		return awserrors.IgnoreNotFound(fmt.Errorf("getting instance profile %q, %w", profileName, err))
	}
	if len(out.InstanceProfile.Roles) > 0 {
		for _, role := range out.InstanceProfile.Roles {
			if _, err = p.iamapi.RemoveRoleFromInstanceProfileWithContext(ctx, &iam.RemoveRoleFromInstanceProfileInput{
				InstanceProfileName: aws.String(profileName),
				RoleName:            role.RoleName,
			}); err != nil {
				return fmt.Errorf("removing role %q from instance profile %q, %w", aws.StringValue(role.RoleName), profileName, err)
			}
		}
	}
	if _, err = p.iamapi.DeleteInstanceProfileWithContext(ctx, &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
	}); err != nil {
		return awserrors.IgnoreNotFound(fmt.Errorf("deleting instance profile %q, %w", profileName, err))
	}
	return nil
}

// GetProfileName gets the string for the profile name based on the cluster name and the NodeClass UUID.
// The length of this string can never exceed the maximum instance profile name limit of 128 characters.
func GetProfileName(ctx context.Context, nodeClass *v1beta1.EC2NodeClass) string {
	return fmt.Sprintf("%s_%s", utils.Truncate(settings.FromContext(ctx).ClusterName, 91), string(nodeClass.UID))
}
