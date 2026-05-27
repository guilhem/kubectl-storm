package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseConfigIncludesKrewLinuxARM64Artifact(t *testing.T) {
	goreleaser, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}
	krew, err := os.ReadFile("../.krew.yaml")
	if err != nil {
		t.Fatalf("read .krew.yaml: %v", err)
	}

	if !strings.Contains(string(goreleaser), "- arm64") {
		t.Fatal(".goreleaser.yaml should build arm64 archives")
	}
	if !strings.Contains(string(krew), "kubectl-storm_{{ .TagName }}_linux_arm64.tar.gz") {
		t.Fatal(".krew.yaml should reference the linux arm64 archive")
	}
}
