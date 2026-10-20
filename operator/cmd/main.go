// Command horus-operator registers annotated Ingresses in a horus DNS server.
//
// Each cluster runs one operator (leader-elected) with its own cluster token.
// The token decides the cluster id (owner prefix) and the single zone the
// cluster may write to; the operator learns both from GET /v1/whoami.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	horus "github.com/warewave/horus/pkg/client"

	"github.com/warewave/horus/operator/internal/controller"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "horus-operator:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		horusURL     string
		tokenFile    string
		caFile       string
		metricsAddr  string
		probeAddr    string
		leaderElect  bool
		gcInterval   time.Duration
		pendingRetry time.Duration
	)
	flag.StringVar(&horusURL, "horus-url", os.Getenv("HORUS_URL"), "horus API base URL, e.g. https://horus.internal:8053")
	flag.StringVar(&tokenFile, "token-file", "/var/run/secrets/horus/token", "file holding the cluster token")
	flag.StringVar(&caFile, "ca-file", "", "PEM CA bundle to verify the horus API certificate (optional)")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint; \"0\" disables")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health/ready probe endpoint")
	flag.BoolVar(&leaderElect, "leader-elect", true, "enable leader election (needed with >1 replica)")
	flag.DurationVar(&gcInterval, "gc-interval", 5*time.Minute, "how often to remove records of vanished Ingresses")
	flag.DurationVar(&pendingRetry, "pending-requeue", 30*time.Second, "re-check interval for Ingresses without an LB address")
	zopts := zap.Options{}
	zopts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zopts)))
	setupLog := ctrl.Log.WithName("setup")

	if horusURL == "" {
		return fmt.Errorf("--horus-url (or HORUS_URL) is required")
	}
	tok, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	var tlsConf *tls.Config
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return fmt.Errorf("read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("no certificates in %s", caFile)
		}
		tlsConf = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	hc, err := horus.New(horusURL, strings.TrimSpace(string(tok)), horus.Options{TLS: tlsConf})
	if err != nil {
		return err
	}

	// Learn our identity; fail fast (Kubernetes restarts us) if horus is
	// unreachable or the token is wrong.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	who, err := hc.WhoAmI(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("whoami: %w", err)
	}
	if who.Admin || who.Cluster == "" {
		return fmt.Errorf("the token is an admin token; the operator needs a cluster token (horus cluster add)")
	}
	setupLog.Info("connected to horus", "version", version, "cluster", who.Cluster, "zone", who.Zone)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "horus-operator.warewave.tech",
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("horus", func(req *http.Request) error {
		_, err := hc.WhoAmI(req.Context())
		return err
	}); err != nil {
		return err
	}

	if err := (&controller.IngressReconciler{
		Client:         mgr.GetClient(),
		Horus:          hc,
		Zone:           who.Zone,
		Recorder:       mgr.GetEventRecorderFor("horus-operator"),
		PendingRequeue: pendingRetry,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup controller: %w", err)
	}
	if err := mgr.Add(&controller.GarbageCollector{
		Reader:   mgr.GetAPIReader(),
		Horus:    hc,
		Interval: gcInterval,
	}); err != nil {
		return err
	}

	setupLog.Info("starting manager")
	return mgr.Start(ctrl.SetupSignalHandler())
}
