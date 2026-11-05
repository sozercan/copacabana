package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// NotationSigner signs with the Notation CLI using a key and certificate chain mounted into the verify Job. The
// signature is what a cluster image integrity policy (Kyverno, Ratify) checks at admission; the DSSE attestation
// remains the controller's own check. insecure lists plain-HTTP registry hosts.
func NotationSigner(keyPath, certPath string, insecure []string) Signer {
	return func(ctx context.Context, pinned string) error {
		dir, err := os.MkdirTemp("", "notation")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		cfg := map[string]any{
			"default": "copa",
			"keys":    []map[string]string{{"name": "copa", "keyPath": keyPath, "certPath": certPath}},
		}
		raw, _ := json.Marshal(cfg)
		if err := os.MkdirAll(filepath.Join(dir, "notation"), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "notation", "signingkeys.json"), raw, 0o600); err != nil {
			return err
		}
		args := []string{"sign", "--key", "copa"}
		host := strings.SplitN(pinned, "/", 2)[0]
		for _, h := range insecure {
			if h == host {
				args = append(args, "--insecure-registry")
			}
		}
		cmd := exec.CommandContext(ctx, "notation", append(args, pinned)...)
		cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("notation sign: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}
