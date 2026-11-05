// Command copa-worker runs the scan, build and verify steps inside Jobs created by copa-rollout-controller.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sozercan/copacabana/internal/attest"
	"github.com/sozercan/copacabana/internal/registry"
	"github.com/sozercan/copacabana/internal/trivy"
	"github.com/sozercan/copacabana/internal/worker"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: copa-worker <scan|build|verify|keygen> [flags]`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var (
		result any
		err    error
	)
	switch os.Args[1] {
	case "scan":
		result, err = runScan(ctx, os.Args[2:])
	case "build":
		result, err = runBuild(ctx, os.Args[2:])
	case "verify":
		result, err = runVerify(ctx, os.Args[2:])
	case "keygen":
		err = runKeygen(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		msg := err.Error()
		fmt.Fprintln(os.Stderr, "error:", msg)
		writeTermination(map[string]string{"error": truncate(msg, 3500)})
		os.Exit(1)
	}
	if result != nil {
		out, _ := json.Marshal(result)
		fmt.Println(string(out))
		writeTermination(result)
	}
}

func writeTermination(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	// Best effort: outside a pod the file may not exist.
	_ = os.WriteFile(worker.TerminationLog, data, 0o644)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type common struct {
	insecure    string
	trivyServer string
	trivyBin    string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.insecure, "insecure-registries", os.Getenv("COPA_INSECURE_REGISTRIES"), "comma-separated plain-HTTP registry hosts")
	fs.StringVar(&c.trivyServer, "trivy-server", os.Getenv("COPA_TRIVY_SERVER"), "Trivy server URL")
	fs.StringVar(&c.trivyBin, "trivy", "trivy", "trivy binary")
}

func (c *common) registry() *registry.Client {
	return registry.New(strings.Split(c.insecure, ","))
}

func (c *common) scanner() trivy.Scanner {
	return trivy.Scanner{Server: c.trivyServer, InsecureRegistries: strings.Split(c.insecure, ","), Binary: c.trivyBin}
}

func runScan(ctx context.Context, args []string) (any, error) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	var c common
	c.register(fs)
	image := fs.String("image", "", "pinned image to scan")
	reportRepo := fs.String("report-repo", "", "repository to push the report to; empty to skip")
	_ = fs.Parse(args)
	if *image == "" {
		return nil, fmt.Errorf("--image is required")
	}
	return worker.Scan(ctx, c.registry(), c.scanner(), *image, *reportRepo)
}

func runBuild(ctx context.Context, args []string) (any, error) {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	var c common
	c.register(fs)
	var o worker.BuildOptions
	fs.StringVar(&o.Source, "source", "", "pinned platform source image")
	fs.StringVar(&o.ReportRef, "report", "", "pinned report artifact")
	fs.StringVar(&o.Target, "target", "", "staging tag to push the patched image to")
	fs.StringVar(&o.BuildKit, "buildkit", "unix:///run/buildkit/buildkitd.sock", "BuildKit address")
	fs.StringVar(&o.CopaBin, "copa", "copa", "copa binary")
	fs.StringVar(&o.WorkDir, "workdir", "/work", "scratch directory")
	fs.DurationVar(&o.Timeout, "timeout", 20*time.Minute, "copa timeout")
	_ = fs.Parse(args)
	if o.Source == "" || o.ReportRef == "" || o.Target == "" {
		return nil, fmt.Errorf("--source, --report and --target are required")
	}
	return worker.Build(ctx, c.registry(), o)
}

func runVerify(ctx context.Context, args []string) (any, error) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	var c common
	c.register(fs)
	specJSON := fs.String("spec", os.Getenv("COPA_VERIFY_SPEC"), "verify spec as JSON")
	keyPath := fs.String("key", "/keys/signing.key", "ed25519 signing key (PEM)")
	notationKey := fs.String("notation-key", "/keys/notation.key", "Notation signing key; signing is skipped if it's missing")
	notationCert := fs.String("notation-cert", "/keys/notation.crt", "Notation certificate chain for --notation-key")
	_ = fs.Parse(args)
	var spec worker.VerifySpec
	if err := json.Unmarshal([]byte(*specJSON), &spec); err != nil {
		return nil, fmt.Errorf("parsing --spec: %w", err)
	}
	keyData, err := os.ReadFile(*keyPath)
	if err != nil {
		return nil, fmt.Errorf("reading signing key: %w", err)
	}
	key, err := attest.ParsePrivateKey(keyData)
	if err != nil {
		return nil, err
	}
	var sign worker.Signer
	if _, err := os.Stat(*notationKey); err == nil {
		sign = worker.NotationSigner(*notationKey, *notationCert, strings.Split(c.insecure, ","))
	}
	return worker.Verify(ctx, c.registry(), c.scanner(), spec, key, sign)
}

func runKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	dir := fs.String("out", ".", "output directory")
	_ = fs.Parse(args)
	priv, pub, err := attest.GenerateKey()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "signing.key"), priv, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "signing.pub"), pub, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s and %s\n", filepath.Join(*dir, "signing.key"), filepath.Join(*dir, "signing.pub"))
	return nil
}
