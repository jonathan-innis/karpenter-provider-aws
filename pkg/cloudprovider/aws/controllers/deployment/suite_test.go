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

package deployment_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	. "github.com/aws/karpenter/pkg/test/expectations"

	"github.com/aws/karpenter/pkg/cloudprovider/aws"
	"github.com/aws/karpenter/pkg/cloudprovider/aws/controllers/deployment"
	awsfake "github.com/aws/karpenter/pkg/cloudprovider/aws/fake"
	"github.com/aws/karpenter/pkg/test"
)

var ctx context.Context
var env *test.Environment
var sqsapi *awsfake.SQSAPI
var sqsProvider *aws.SQSProvider
var eventbridgeapi *awsfake.EventBridgeAPI
var eventBridgeProvider *aws.EventBridgeProvider
var recorder *awsfake.EventRecorder
var controller *deployment.Controller

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "AWS Karpenter Deployment")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(ctx, func(e *test.Environment) {
		recorder = awsfake.NewEventRecorder()
		metadata := aws.NewMetadata("us-east-1", "000000000000")

		sqsapi = &awsfake.SQSAPI{}
		eventbridgeapi = &awsfake.EventBridgeAPI{}
		sqsProvider = aws.NewSQSProvider(ctx, sqsapi, metadata)
		eventBridgeProvider = aws.NewEventBridgeProvider(eventbridgeapi, metadata, sqsProvider.QueueName())
	})
	Expect(env.Start()).To(Succeed(), "Failed to start environment")
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	sqsapi.Reset()
	eventbridgeapi.Reset()
	controller = deployment.NewController(env.Client, nil, recorder, sqsProvider, eventBridgeProvider)
})
var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})
