// Command copa-rollout-controller finds the images opted-in workloads run, builds verified Copa patches for them
// in isolated Jobs, and rolls approved patches out through admission.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/sozercan/copacabana/api/v1alpha1"
	"github.com/sozercan/copacabana/internal/attest"
	"github.com/sozercan/copacabana/internal/controller"
	"github.com/sozercan/copacabana/internal/registry"
)

func main() {
	var (
		cfg        controller.Config
		platforms  string
		insecure   string
		pubKeyPath string
		metrics    string
		probes     string
		leader     bool
	)
	flag.StringVar(&cfg.Mode, "mode", controller.ModeBuilder, "report-only, builder, or consumer")
	flag.StringVar(&platforms, "platforms", "linux/amd64", "comma-separated platforms every patch must cover")
	flag.StringVar(&insecure, "insecure-registries", "", "comma-separated plain-HTTP registry hosts")
	flag.StringVar(&cfg.PatchedPrefix, "patched-prefix", "", "registry prefix for verified patches, e.g. myacr.azurecr.io/patched")
	flag.StringVar(&cfg.StagingPrefix, "staging-prefix", "", "registry prefix build Jobs push to, e.g. myacr.azurecr.io/staging")
	flag.StringVar(&cfg.SystemNamespace, "system-namespace", "copa-system", "namespace the controller runs in")
	flag.StringVar(&cfg.BuilderNamespace, "builder-namespace", "copa-builder", "namespace for PatchBuilds and scan and build Jobs")
	flag.StringVar(&cfg.VerifierNamespace, "verifier-namespace", "copa-verifier", "namespace for verify Jobs")
	flag.StringVar(&cfg.WorkerImage, "worker-image", "", "copa-worker image")
	flag.StringVar(&cfg.BuildKitImage, "buildkit-image", "moby/buildkit:v0.33.0-rootless", "rootless BuildKit image for build Jobs")
	flag.StringVar(&cfg.TrivyServer, "trivy-server", "http://trivy-server.copa-builder:4954", "Trivy server URL")
	flag.StringVar(&pubKeyPath, "attestation-public-key", "/keys/signing.pub", "public key that verifies patch attestations")
	flag.DurationVar(&cfg.ResolveInterval, "resolve-interval", 2*time.Minute, "how often to re-resolve tags, which is how tag drift is noticed")
	flag.DurationVar(&cfg.RescanInterval, "rescan-interval", 24*time.Hour, "how often to rescan each source for new fixes")
	flag.IntVar(&cfg.MaxConcurrentBuilds, "max-concurrent-builds", 4, "PatchBuilds in flight at once")
	flag.DurationVar(&cfg.BuildTimeout, "build-timeout", 30*time.Minute, "deadline for each scan, build or verify Job")
	flag.DurationVar(&cfg.RolloutDeadline, "rollout-deadline", 10*time.Minute, "how long a restarted Deployment has to become ready")
	flag.DurationVar(&cfg.HealthyFor, "healthy-for", 2*time.Minute, "how long new pods must stay healthy before a workload counts as done")
	flag.DurationVar(&cfg.Soak, "soak", 24*time.Hour, "how long patched workloads must run healthy before the patch is known-good")
	flag.IntVar(&cfg.MaxConcurrentRestarts, "max-concurrent-restarts", 3, "Deployments restarting at once across the cluster")
	flag.BoolVar(&cfg.RequireSafeStrategy, "require-safe-strategy", true, "skip Deployments without maxUnavailable 0, surge, and readiness probes")
	flag.BoolVar(&cfg.AllowEmulation, "allow-emulation", false, "build platforms without matching nodes under qemu on the builder pool")
	flag.BoolVar(&cfg.AllowNewVulnerabilities, "allow-new-vulnerabilities", false, "let verification pass when patching adds vulnerabilities")
	flag.StringVar(&metrics, "metrics-bind-address", ":8080", "metrics address")
	flag.StringVar(&probes, "health-probe-bind-address", ":8081", "health probe address")
	flag.BoolVar(&leader, "leader-elect", true, "elect a leader so only one replica reconciles at a time")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	cfg.Platforms = splitList(platforms)
	cfg.InsecureRegistries = splitList(insecure)
	if err := validate(&cfg); err != nil {
		log.Error(err, "invalid configuration")
		os.Exit(2)
	}
	if cfg.Mode != controller.ModeReportOnly || pubKeyPath != "" {
		data, err := os.ReadFile(pubKeyPath)
		if err != nil {
			log.Error(err, "reading attestation public key")
			os.Exit(2)
		}
		if cfg.PublicKey, err = attest.ParsePublicKey(data); err != nil {
			log.Error(err, "parsing attestation public key")
			os.Exit(2)
		}
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metrics},
		HealthProbeBindAddress: probes,
		LeaderElection:         leader,
		LeaderElectionID:       "copa-rollout-controller.copa.sh",
		// A crashed leader's lease is released on shutdown so a standby takes over within seconds.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		log.Error(err, "creating manager")
		os.Exit(1)
	}
	_ = mgr.AddHealthzCheck("ping", func(_ *http.Request) error { return nil })
	_ = mgr.AddReadyzCheck("ping", func(_ *http.Request) error { return nil })

	reg := registry.New(cfg.InsecureRegistries)
	rec := mgr.GetEventRecorder("copa-rollout-controller")
	c := mgr.GetClient()

	must := func(name string, err error) {
		if err != nil {
			log.Error(err, "setting up controller", "controller", name)
			os.Exit(1)
		}
	}
	must("inventory", (&controller.InventoryReconciler{Client: c, Config: &cfg, Registry: reg, Recorder: rec}).SetupWithManager(mgr))
	must("sourceimage", (&controller.SourceImageReconciler{
		Client: c, Config: &cfg, Catalog: &controller.Catalog{Registry: reg, Config: &cfg}, Recorder: rec,
	}).SetupWithManager(mgr))
	if cfg.Mode != controller.ModeConsumer {
		must("patchbuild", (&controller.PatchBuildReconciler{Client: c, Config: &cfg, Recorder: rec}).SetupWithManager(mgr))
	}
	if cfg.Mode != controller.ModeReportOnly {
		must("rollout", (&controller.RolloutReconciler{Client: c, Config: &cfg, Recorder: rec}).SetupWithManager(mgr))
		must("missedswap", (&controller.MissedSwapReconciler{Client: c, Recorder: rec}).SetupWithManager(mgr))
	}

	log.Info("starting", "mode", cfg.Mode, "platforms", cfg.Platforms, "patched", cfg.PatchedPrefix)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validate(cfg *controller.Config) error {
	switch cfg.Mode {
	case controller.ModeReportOnly, controller.ModeBuilder, controller.ModeConsumer:
	default:
		return fmt.Errorf("unknown --mode %q", cfg.Mode)
	}
	if cfg.PatchedPrefix == "" {
		return fmt.Errorf("--patched-prefix is required")
	}
	if cfg.Mode != controller.ModeConsumer && (cfg.StagingPrefix == "" || cfg.WorkerImage == "") {
		return fmt.Errorf("--staging-prefix and --worker-image are required in %s mode", cfg.Mode)
	}
	if len(cfg.Platforms) == 0 {
		return fmt.Errorf("--platforms is required")
	}
	return nil
}
