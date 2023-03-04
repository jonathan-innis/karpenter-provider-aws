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

package test

import (
	"context"
	"net"

	"github.com/aws/aws-sdk-go/aws/session"
	clock "k8s.io/utils/clock/testing"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"

	"github.com/aws/aws-sdk-go/awstesting/mock"
	"github.com/imdario/mergo"
	"github.com/patrickmn/go-cache"

	"github.com/aws/karpenter-core/pkg/test"

	awscache "github.com/aws/karpenter/pkg/cache"
	"github.com/aws/karpenter/pkg/fake"
	"github.com/aws/karpenter/pkg/providers/amifamily"
	"github.com/aws/karpenter/pkg/providers/instance"
	"github.com/aws/karpenter/pkg/providers/instancetype"
	"github.com/aws/karpenter/pkg/providers/launchtemplate"
	"github.com/aws/karpenter/pkg/providers/pricing"
	"github.com/aws/karpenter/pkg/providers/securitygroup"
	"github.com/aws/karpenter/pkg/providers/subnet"
)

type Environment struct {
	*EnvironmentOptions
	Session                *session.Session
	SubnetProvider         *subnet.Provider
	SecurityGroupProvider  *securitygroup.Provider
	AMIProvider            *amifamily.Provider
	AMIResolver            *amifamily.Resolver
	LaunchTemplateProvider *launchtemplate.Provider
	PricingProvider        *pricing.Provider
	InstanceTypesProvider  *instancetype.Provider
	InstanceProvider       *instance.Provider
}

func (c *Environment) Reset() {
	c.EnvironmentOptions.Reset()
}

type EnvironmentOptions struct {
	Clock                     *clock.FakeClock
	SSMCache                  *cache.Cache
	EC2Cache                  *cache.Cache
	KubernetesVersionCache    *cache.Cache
	InstanceTypeCache         *cache.Cache
	UnavailableOfferingsCache *awscache.UnavailableOfferings
	LaunchTemplateCache       *cache.Cache
	SubnetCache               *cache.Cache
	SecurityGroupCache        *cache.Cache
	EC2API                    *fake.EC2API
	EKSAPI                    *fake.EKSAPI
	SSMAPI                    *fake.SSMAPI
	PricingAPI                *fake.PricingAPI
}

func (co *EnvironmentOptions) Reset() {
	co.SSMCache.Flush()
	co.EC2Cache.Flush()
	co.KubernetesVersionCache.Flush()
	co.InstanceTypeCache.Flush()
	co.UnavailableOfferingsCache.Flush()
	co.LaunchTemplateCache.Flush()
	co.SubnetCache.Flush()
	co.SecurityGroupCache.Flush()
	co.EC2API.Reset()
	co.EKSAPI.Reset()
	co.SSMAPI.Reset()
	co.PricingAPI.Reset()
}

func NewEnvironment(ctx context.Context, env *test.Environment, overrides ...EnvironmentOptions) *Environment {
	options := &EnvironmentOptions{}
	for _, override := range overrides {
		if err := mergo.Merge(options, override, mergo.WithOverride); err != nil {
			logging.FromContext(ctx).Fatalf("merging settings, %s", err)
		}
	}
	options.SSMAPI = OptionOR[*fake.SSMAPI](options.SSMAPI, &fake.SSMAPI{})
	options.EC2API = OptionOR[*fake.EC2API](options.EC2API, &fake.EC2API{})
	options.SSMCache = OptionOR[*cache.Cache](options.SSMCache, cache.New(awscache.DefaultTTL, awscache.DefaultCleanupInterval))

	// Providers
	pricingProvider := pricing.NewProvider(ctx, options.PricingAPI, options.EC2API, "", make(chan struct{}))
	subnetProvider := subnet.NewProvider(options.EC2API, options.SubnetCache)
	securityGroupProvider := securitygroup.NewProvider(options.EC2API, options.SecurityGroupCache)
	amiProvider := amifamily.NewProvider(env.Client, env.KubernetesInterface, options.SSMAPI, options.EC2API, options.SSMCache, options.EC2Cache, options.KubernetesVersionCache)
	amiResolver := amifamily.New(env.Client, amiProvider)
	instanceTypesProvider := instancetype.NewProvider("", options.InstanceTypeCache, options.EC2API, subnetProvider, options.UnavailableOfferingsCache, pricingProvider)
	launchTemplateProvider :=
		launchtemplate.NewProvider(
			ctx,
			options.LaunchTemplateCache,
			options.EC2API,
			amiResolver,
			securityGroupProvider,
			ptr.String("ca-bundle"),
			make(chan struct{}),
			net.ParseIP("10.0.100.10"),
			"https://test-cluster",
		)
	instanceProvider :=
		instance.NewProvider(ctx,
			"",
			options.EC2API,
			options.UnavailableOfferingsCache,
			instanceTypesProvider,
			subnetProvider,
			launchTemplateProvider,
		)

	return &Environment{
		EnvironmentOptions:     options,
		Session:                mock.Session,
		InstanceTypesProvider:  instanceTypesProvider,
		InstanceProvider:       instanceProvider,
		SubnetProvider:         subnetProvider,
		SecurityGroupProvider:  securityGroupProvider,
		PricingProvider:        pricingProvider,
		AMIProvider:            amiProvider,
		AMIResolver:            amiResolver,
		LaunchTemplateProvider: launchTemplateProvider,
	}
}

func OptionOR[T any](x T, fallback T) T {
	if x == nil {
		return fallback
	}
	return x
}
