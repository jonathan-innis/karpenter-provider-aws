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

package controllers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/utils/clock"
	"knative.dev/pkg/configmap/informer"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/system"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter/pkg/apis"
	"github.com/aws/karpenter/pkg/cloudprovider"
	cloudprovidermetrics "github.com/aws/karpenter/pkg/cloudprovider/metrics"
	"github.com/aws/karpenter/pkg/config"
	"github.com/aws/karpenter/pkg/controllers/consolidation"
	"github.com/aws/karpenter/pkg/controllers/counter"
	metricspod "github.com/aws/karpenter/pkg/controllers/metrics/pod"
	metricsprovisioner "github.com/aws/karpenter/pkg/controllers/metrics/provisioner"
	metricsstate "github.com/aws/karpenter/pkg/controllers/metrics/state"
	"github.com/aws/karpenter/pkg/controllers/node"
	"github.com/aws/karpenter/pkg/controllers/provisioning"
	"github.com/aws/karpenter/pkg/controllers/state"
	"github.com/aws/karpenter/pkg/controllers/termination"
	"github.com/aws/karpenter/pkg/events"
	"github.com/aws/karpenter/pkg/metrics"
	"github.com/aws/karpenter/pkg/utils/injection"
	"github.com/aws/karpenter/pkg/utils/options"
	"github.com/aws/karpenter/pkg/utils/project"
)

var (
	scheme    = runtime.NewScheme()
	component = "controller"
	appName   = "karpenter"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apis.AddToScheme(scheme))
	metrics.MustRegister() // Registers cross-controller metrics
}

type ControllerInitFunc func(context.Context, *ControllerOptions) <-chan struct{}

// Controller is an interface implemented by Karpenter custom resources.
type Controller interface {
	// Reconcile hands a hydrated kubernetes resource to the controller for
	// reconciliation. Any changes made to the resource's status are persisted
	// after Reconcile returns, even if it returns an error.
	Reconcile(context.Context, reconcile.Request) (reconcile.Result, error)
	// Register will register the controller with the manager
	Register(context.Context, manager.Manager) error
}

type ControllerOptions struct {
	BaseContext func() context.Context
	Cluster     *state.Cluster
	KubeClient  client.Client
	Recorder    events.Recorder
	Clock       clock.Clock

	StartAsync   <-chan struct{}
	CleanupAsync <-chan struct{}
}

func Initialize(injectCloudProvider func(context.Context, cloudprovider.Options) (cloudprovider.CloudProvider, ControllerInitFunc)) {
	opts := options.New().MustParse()
	// Setup Client
	controllerRuntimeConfig := controllerruntime.GetConfigOrDie()
	controllerRuntimeConfig.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(float32(opts.KubeClientQPS), opts.KubeClientBurst)
	controllerRuntimeConfig.UserAgent = appName
	clientSet := kubernetes.NewForConfigOrDie(controllerRuntimeConfig)

	// Set up logger and watch for changes to log level
	cmw := informer.NewInformedWatcher(clientSet, system.Namespace())
	ctx := injection.LoggingContextOrDie(component, controllerRuntimeConfig, cmw)
	ctx = newRunnableContext(controllerRuntimeConfig, opts, logging.FromContext(ctx))()
	ctx, cancel := context.WithCancel(ctx)

	// Setup the cleanup logic for teardown on SIGINT or SIGTERM
	cleanup := make(chan struct{}) // This is a channel to broadcast to controllers cleanup can start
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		logging.FromContext(context.Background()).Infof("Got a signal to react to")
		close(cleanup)
		cancel()
	}()

	logging.FromContext(ctx).Infof("Initializing with version %s", project.Version)

	if opts.MemoryLimit > 0 {
		newLimit := int64(float64(opts.MemoryLimit) * 0.9)
		logging.FromContext(ctx).Infof("Setting GC memory limit to %d, container limit = %d", newLimit, opts.MemoryLimit)
		debug.SetMemoryLimit(newLimit)
	}

	manager := NewManagerOrDie(ctx, controllerRuntimeConfig, controllerruntime.Options{
		Logger:                     ignoreDebugEvents(zapr.NewLogger(logging.FromContext(ctx).Desugar())),
		LeaderElection:             opts.EnableLeaderElection,
		LeaderElectionID:           "karpenter-leader-election",
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		Scheme:                     scheme,
		MetricsBindAddress:         fmt.Sprintf(":%d", opts.MetricsPort),
		HealthProbeBindAddress:     fmt.Sprintf(":%d", opts.HealthProbePort),
		BaseContext:                newRunnableContext(controllerRuntimeConfig, opts, logging.FromContext(ctx)),
	})

	if opts.EnableProfiling {
		utilruntime.Must(registerPprof(manager))
	}
	cloudProvider, injectControllers := injectCloudProvider(ctx, cloudprovider.Options{ClientSet: clientSet, KubeClient: manager.GetClient(), StartAsync: manager.Elected()})
	if hp, ok := cloudProvider.(HealthCheck); ok {
		utilruntime.Must(manager.AddHealthzCheck("cloud-provider", hp.LivenessProbe))
	}
	cloudProvider = cloudprovidermetrics.Decorate(cloudProvider)

	cfg, err := config.New(ctx, clientSet, cmw)
	if err != nil {
		// this does not happen if the config map is missing or invalid, only if some other error occurs
		logging.FromContext(ctx).Fatalf("unable to load config, %s", err)
	}

	if err := cmw.Start(ctx.Done()); err != nil {
		logging.FromContext(ctx).Errorf("watching configmaps, config changes won't be applied immediately, %s", err)
	}

	realClock := clock.RealClock{}
	recorder := events.NewRecorder(manager.GetEventRecorderFor(appName))
	recorder = events.NewLoadSheddingRecorder(recorder)
	recorder = events.NewDedupeRecorder(recorder)

	cluster := state.NewCluster(realClock, cfg, manager.GetClient(), cloudProvider)
	provisioner := provisioning.NewProvisioner(ctx, cfg, manager.GetClient(), clientSet.CoreV1(), recorder, cloudProvider, cluster)
	consolidation.NewController(ctx, realClock, manager.GetClient(), provisioner, cloudProvider, recorder, cluster, manager.Elected())

	// Inject cloudprovider-specific controllers into the controller-set using the injectControllers function
	// Inject the base cloud provider into the injection function rather than the decorated interface
	controllerOptions := &ControllerOptions{
		BaseContext:  newRunnableContext(controllerRuntimeConfig, opts, logging.FromContext(ctx)),
		Cluster:      cluster,
		KubeClient:   manager.GetClient(),
		Recorder:     recorder,
		StartAsync:   manager.Elected(),
		CleanupAsync: cleanup,
		Clock:        realClock,
	}
	done := injectControllers(ctx, controllerOptions)

	metricsstate.StartMetricScraper(ctx, cluster)

	if err := RegisterControllers(ctx,
		manager,
		provisioning.NewController(manager.GetClient(), provisioner, recorder),
		state.NewNodeController(manager.GetClient(), cluster),
		state.NewPodController(manager.GetClient(), cluster),
		state.NewProvisionerController(manager.GetClient(), cluster),
		node.NewController(realClock, manager.GetClient(), cloudProvider, cluster),
		termination.NewController(ctx, realClock, manager.GetClient(), clientSet.CoreV1(), recorder, cloudProvider),
		metricspod.NewController(manager.GetClient()),
		metricsprovisioner.NewController(manager.GetClient()),
		counter.NewController(manager.GetClient(), cluster),
	).Start(ctx); err != nil {
		panic(fmt.Sprintf("Unable to start manager, %s", err))
	}
	<-done
}

// NewManagerOrDie instantiates a controller manager or panics
func NewManagerOrDie(ctx context.Context, config *rest.Config, options controllerruntime.Options) manager.Manager {
	newManager, err := controllerruntime.NewManager(config, options)
	if err != nil {
		panic(fmt.Sprintf("Failed to create controller newManager, %s", err))
	}
	if err := newManager.GetFieldIndexer().IndexField(ctx, &v1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*v1.Pod).Spec.NodeName}
	}); err != nil {
		panic(fmt.Sprintf("Failed to setup pod indexer, %s", err))
	}
	return newManager
}

type HealthCheck interface {
	LivenessProbe(req *http.Request) error
}

// RegisterControllers registers a set of controllers to the controller manager
func RegisterControllers(ctx context.Context, m manager.Manager, controllers ...Controller) manager.Manager {
	for _, c := range controllers {
		if err := c.Register(ctx, m); err != nil {
			panic(err)
		}
		// if the controller implements a liveness check, connect it
		if lp, ok := c.(HealthCheck); ok {
			utilruntime.Must(m.AddHealthzCheck(fmt.Sprintf("%T", c), lp.LivenessProbe))
		}
	}
	if err := m.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		panic(fmt.Sprintf("Failed to add health probe, %s", err))
	}
	if err := m.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		panic(fmt.Sprintf("Failed to add ready probe, %s", err))
	}
	return m
}

func registerPprof(manager manager.Manager) error {
	for path, handler := range map[string]http.Handler{
		"/debug/pprof/":             http.HandlerFunc(pprof.Index),
		"/debug/pprof/cmdline":      http.HandlerFunc(pprof.Cmdline),
		"/debug/pprof/profile":      http.HandlerFunc(pprof.Profile),
		"/debug/pprof/symbol":       http.HandlerFunc(pprof.Symbol),
		"/debug/pprof/trace":        http.HandlerFunc(pprof.Trace),
		"/debug/pprof/allocs":       pprof.Handler("allocs"),
		"/debug/pprof/heap":         pprof.Handler("heap"),
		"/debug/pprof/block":        pprof.Handler("block"),
		"/debug/pprof/goroutine":    pprof.Handler("goroutine"),
		"/debug/pprof/threadcreate": pprof.Handler("threadcreate"),
	} {
		err := manager.AddMetricsExtraHandler(path, handler)
		if err != nil {
			return err
		}
	}
	return nil
}

type ignoreDebugEventsSink struct {
	name string
	sink logr.LogSink
}

func (i ignoreDebugEventsSink) Init(ri logr.RuntimeInfo) {
	i.sink.Init(ri)
}
func (i ignoreDebugEventsSink) Enabled(level int) bool { return i.sink.Enabled(level) }
func (i ignoreDebugEventsSink) Info(level int, msg string, keysAndValues ...interface{}) {
	// ignore debug "events" logs
	if level == 1 && i.name == "events" {
		return
	}
	i.sink.Info(level, msg, keysAndValues...)
}
func (i ignoreDebugEventsSink) Error(err error, msg string, keysAndValues ...interface{}) {
	i.sink.Error(err, msg, keysAndValues...)
}
func (i ignoreDebugEventsSink) WithValues(keysAndValues ...interface{}) logr.LogSink {
	return i.sink.WithValues(keysAndValues...)
}
func (i ignoreDebugEventsSink) WithName(name string) logr.LogSink {
	return &ignoreDebugEventsSink{name: name, sink: i.sink.WithName(name)}
}

// ignoreDebugEvents wraps the logger with one that ignores any debug logs coming from a logger named "events".  This
// prevents every event we write from creating a debug log which spams the log file during scale-ups due to recording
// pod scheduling decisions as events for visibility.
func ignoreDebugEvents(logger logr.Logger) logr.Logger {
	return logr.New(&ignoreDebugEventsSink{sink: logger.GetSink()})
}

func Cleanup() <-chan os.Signal {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT)
	return c
}

func newRunnableContext(config *rest.Config, options *options.Options, logger *zap.SugaredLogger) func() context.Context {
	return func() context.Context {
		ctx := context.Background()
		ctx = logging.WithLogger(ctx, logger)
		ctx = injection.WithConfig(ctx, config)
		ctx = injection.WithOptions(ctx, *options)
		return ctx
	}
}
