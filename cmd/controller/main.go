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
	"context"

	"knative.dev/pkg/logging"

	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/cloudprovider/aws"
	awscontrollers "github.com/aws/karpenter/pkg/cloudprovider/aws/controllers"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/controllers/infrastructure"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/controllers/notification"
	"github.com/aws/karpenter/pkg/controllers"
)

func main() {
	controllers.Initialize(func(ctx context.Context, options cloudprovider.Options) (cloudprovider.CloudProvider, func(context.Context, *controllers.ControllerOptions)) {
		provider := aws.NewCloudProvider(ctx, options)
		injectControllers := func(ctx context.Context, opts *controllers.ControllerOptions) {
			recorder := awscontrollers.NewRecorder(opts.Recorder)
			ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named("aws"))

			// Injecting the controllers that will start when opts.StartAsync is triggered
			notification.NewController(ctx, opts.Clock, opts.KubeClient, provider.SQSProvider, recorder, opts.Provisioner, opts.Cluster, opts.StartAsync)
			infrastructure.NewController(ctx, opts.Clock, opts.KubeClient, recorder, opts.Cluster, opts.StartAsync)
		}
		return provider, injectControllers
	})
}
