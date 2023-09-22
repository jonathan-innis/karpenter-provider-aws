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

package main

import (
	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1beta1 "github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/controllers"
	"github.com/aws/karpenter/pkg/operator"
	"github.com/aws/karpenter/pkg/webhooks"

	"github.com/aws/karpenter-core/pkg/cloudprovider/metrics"
	corecontrollers "github.com/aws/karpenter-core/pkg/controllers"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	coreoperator "github.com/aws/karpenter-core/pkg/operator"
	corewebhooks "github.com/aws/karpenter-core/pkg/webhooks"
)

func main() {
	ctx, op := operator.NewOperator(coreoperator.NewOperator())
	awsCloudProvider := cloudprovider.New(
		op.InstanceTypesProvider,
		op.InstanceProvider,
		op.EventRecorder,
		op.GetClient(),
		op.AMIProvider,
		op.SecurityGroupProvider,
		op.SubnetProvider,
	)
	lo.Must0(op.Manager.GetFieldIndexer().IndexField(ctx, &corev1beta1.NodeClaim{}, "spec.nodeClass.name", func(o client.Object) []string {
		nc := o.(*corev1beta1.NodeClaim)
		if nc.Spec.NodeClass == nil {
			return []string{""}
		}
		return []string{nc.Spec.NodeClass.Name}
	}), "failed to setup nodeclaim nodeclass name indexer")
	lo.Must0(op.AddHealthzCheck("cloud-provider", awsCloudProvider.LivenessProbe))
	cloudProvider := metrics.Decorate(awsCloudProvider)

	op.
		WithControllers(ctx, corecontrollers.NewControllers(
			ctx,
			op.Clock,
			op.GetClient(),
			op.KubernetesInterface,
			state.NewCluster(op.Clock, op.GetClient(), cloudProvider),
			op.EventRecorder,
			cloudProvider,
		)...).
		WithWebhooks(ctx, corewebhooks.NewWebhooks()...).
		WithControllers(ctx, controllers.NewControllers(
			ctx,
			op.Session,
			op.Clock,
			op.GetClient(),
			op.EventRecorder,
			op.UnavailableOfferingsCache,
			awsCloudProvider,
			op.SubnetProvider,
			op.SecurityGroupProvider,
			op.InstanceProvider,
			op.InstanceProfileProvider,
			op.PricingProvider,
			op.AMIProvider,
		)...).
		WithWebhooks(ctx, webhooks.NewWebhooks()...).
		Start(ctx)
}
