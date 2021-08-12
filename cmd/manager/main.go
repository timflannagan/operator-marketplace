package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/sirupsen/logrus"

	apiconfigv1 "github.com/openshift/api/config/v1"

	"github.com/operator-framework/operator-marketplace/pkg/apis"
	configv1 "github.com/operator-framework/operator-marketplace/pkg/apis/config/v1"
	olmv1alpha1 "github.com/operator-framework/operator-marketplace/pkg/apis/olm/v1alpha1"
	"github.com/operator-framework/operator-marketplace/pkg/controller"
	"github.com/operator-framework/operator-marketplace/pkg/controller/options"
	"github.com/operator-framework/operator-marketplace/pkg/defaults"
	"github.com/operator-framework/operator-marketplace/pkg/metrics"
	"github.com/operator-framework/operator-marketplace/pkg/operatorhub"
	"github.com/operator-framework/operator-marketplace/pkg/signals"
	"github.com/operator-framework/operator-marketplace/pkg/status"
	sourceCommit "github.com/operator-framework/operator-marketplace/pkg/version"

	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	sdkVersion "github.com/operator-framework/operator-sdk/version"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	// TODO(tflannag): Should this be configurable?
	defaultLeaderElectionConfigMapName = "marketplace-operator-lock"
	defaultRetryPeriod                 = 30 * time.Second
	defaultRenewDeadline               = 60 * time.Second
	defaultLeaseDuration               = 90 * time.Second
)

func printVersion() {
	logrus.Printf("Go Version: %s", runtime.Version())
	logrus.Printf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH)
	logrus.Printf("operator-sdk Version: %v", sdkVersion.Version)
}

func setupScheme() *kruntime.Scheme {
	scheme := kruntime.NewScheme()

	utilruntime.Must(apis.AddToScheme(scheme))
	utilruntime.Must(olmv1alpha1.AddToScheme(scheme))
	utilruntime.Must(v1beta1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	if configv1.IsAPIAvailable() {
		utilruntime.Must(apiconfigv1.AddToScheme(scheme))
	}

	return scheme
}

func main() {
	printVersion()

	var (
		clusterOperatorName     string
		tlsKeyPath              string
		tlsCertPath             string
		leaderElectionNamespace string
		version                 bool
	)
	flag.StringVar(&clusterOperatorName, "clusterOperatorName", "", "configures the name of the OpenShift ClusterOperator that should reflect this operator's status, or the empty string to disable ClusterOperator updates")
	flag.StringVar(&defaults.Dir, "defaultsDir", "", "configures the directory where the default CatalogSources are stored")
	flag.BoolVar(&version, "version", false, "displays marketplace source commit info.")
	flag.StringVar(&tlsKeyPath, "tls-key", "", "Path to use for private key (requires tls-cert)")
	flag.StringVar(&tlsCertPath, "tls-cert", "", "Path to use for certificate (requires tls-key)")
	flag.StringVar(&leaderElectionNamespace, "leader-namespace", "openshift-marketplace", "configures the namespace that will contain the leader election lock")
	flag.Parse()

	logger := logrus.New()

	// Check if version flag was set
	if version {
		logger.Infof("%s", sourceCommit.String())
		os.Exit(0)
	}

	// set TLS to serve metrics over a secure channel if cert is provided
	// cert is provided by default by the marketplace-trusted-ca volume mounted as part of the marketplace-operator deployment
	err := metrics.ServePrometheus(tlsCertPath, tlsKeyPath)
	if err != nil {
		logger.Fatalf("failed to serve prometheus metrics: %s", err)
	}

	namespace, err := k8sutil.GetWatchNamespace()
	if err != nil {
		logger.Fatalf("failed to get watch namespace: %v", err)
	}

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		logger.Fatal(err)
	}

	// Set OpenShift config API availability
	err = configv1.SetConfigAPIAvailability(cfg)
	if err != nil {
		logger.Fatal(err)
	}

	logger.Info("setting up scheme")
	scheme := setupScheme()

	// Even though we are asking to watch all namespaces, we only handle events
	// from the operator's namespace. The reason for watching all namespaces is
	// watch for CatalogSources in targetNamespaces being deleted and recreate
	// them.
	//
	// Note(tflannag): Setting the `MetricsBindAddress` to `0` here disables the
	// metrics listener from controller-runtime. Previously, this was disabled by
	// default in <v0.2.0, but it's now enabled by default and the default port
	// conflicts with the same port we bind for the health checks.
	mgr, err := manager.New(cfg, manager.Options{
		Namespace:          "",
		MetricsBindAddress: "0",
		Scheme:             scheme,
	})
	if err != nil {
		logger.Fatal(err)
	}

	logger.Info("setting up health checks")
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	go http.ListenAndServe(":8080", nil)

	ctx := signals.Context()
	stopCh := ctx.Done()

	leaderCtx, leaderCancel := context.WithCancel(context.Background())
	defer leaderCancel()

	run := func(ctx context.Context) {
		logger.Info("registering components")
		var statusReporter status.Reporter = &status.NoOpReporter{}
		if clusterOperatorName != "" {
			statusReporter, err = status.NewReporter(cfg, mgr, namespace, clusterOperatorName, os.Getenv("RELEASE_VERSION"), stopCh)
			if err != nil {
				logger.Fatal(err)
			}
		}

		// Populate the global default OperatorSources definition and config
		if err := defaults.PopulateGlobals(); err != nil {
			logger.Fatal(err)
		}

		logger.Info("setting up controllers")
		if err := controller.AddToManager(mgr, options.ControllerOptions{}); err != nil {
			logger.Fatal(err)
		}

		if err := ensureDefaults(cfg, mgr.GetScheme()); err != nil {
			logger.Fatalf("failed to setup the default catalogsource manifests: %v", err)
		}

		logger.Info("starting manager")
		if err := mgr.Start(stopCh); err != nil {
			logger.WithError(err).Error("unable to run manager")
		}

		// statusReportingDoneCh will be closed after the operator has successfully stopped reporting ClusterOperator status.
		statusReportingDoneCh := statusReporter.StartReporting()
		// Wait for ClusterOperator status reporting routine to close the statusReportingDoneCh channel.
		<-statusReportingDoneCh
	}

	client, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		logger.Fatal(fmt.Errorf("failed to initialize the kubernetes clientset: %v", err))
	}

	id := os.Getenv("POD_NAME")
	if id == "" {
		logger.Info("failed to determine $POD_NAME falling back to hostname")
		id, err = os.Hostname()
		if err != nil {
			logger.Fatal(err)
		}
	}

	rl := &resourcelock.ConfigMapLock{
		Client: client.CoreV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
		ConfigMapMeta: v1.ObjectMeta{
			Name:      defaultLeaderElectionConfigMapName,
			Namespace: leaderElectionNamespace,
		},
	}
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            rl,
		ReleaseOnCancel: true,
		LeaseDuration:   defaultLeaseDuration,
		RenewDeadline:   defaultRenewDeadline,
		RetryPeriod:     defaultRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				logger.Infof("became leader: %s", id)
				run(leaderCtx)
			},
			OnStoppedLeading: func() {
				logger.Warnf("leader election lost for %s identity", id)
			},
			OnNewLeader: func(identity string) {
				if identity == id {
					return
				}
				logger.Infof("new leader has been elected: %s", identity)
			},
		},
	})
}

// ensureDefaults ensures that all the default OperatorSources are present on
// the cluster
func ensureDefaults(cfg *rest.Config, scheme *kruntime.Scheme) error {
	// The default client serves read requests from the cache which only gets
	// initialized after mgr.Start(). So we need to instantiate a new client
	// for the defaults handler.
	clientForDefaults, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		logrus.Errorf("Error initializing client for handling defaults - %v", err)
		return err
	}

	if configv1.IsAPIAvailable() {
		// Check if the cluster OperatorHub config resource is present.
		operatorHubCluster := &apiconfigv1.OperatorHub{}
		err = clientForDefaults.Get(context.TODO(), client.ObjectKey{Name: operatorhub.DefaultName}, operatorHubCluster)

		// The default OperatorHub config resource is present which will take care of ensuring defaults
		if err == nil {
			return nil
		}
	}

	// Ensure that the default OperatorSources are present based on the definitions
	// in the defaults directory
	result := defaults.New(defaults.GetGlobals()).EnsureAll(clientForDefaults)
	if len(result) != 0 {
		return fmt.Errorf("[defaults] Error ensuring default OperatorSource(s) - %v", result)
	}

	return nil
}
