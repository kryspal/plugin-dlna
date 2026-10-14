package main

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kryspal/plugin-sdk/plugintest"
)

// TestConformance builds the plugin and runs the SDK conformance suite against the binary.
func TestConformance(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "plugin")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	plugintest.Run(t, bin, plugintest.Options{})
}
