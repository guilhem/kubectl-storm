package cmd

import "testing"

func TestValidateOptionsDefaults(t *testing.T) {
	if err := validateOptions(runOptions{
		runDuration:     defaultRunDuration,
		changeThreshold: defaultChangeThreshold,
		signal:          signalResourceVersion,
	}); err != nil {
		t.Fatalf("validateOptions() error = %v", err)
	}
}

func TestValidateOptionsRejectsInvalidSignal(t *testing.T) {
	err := validateOptions(runOptions{
		changeThreshold: 1,
		signal:          changeSignal("invalid"),
	})
	if err == nil {
		t.Fatal("validateOptions() should reject invalid signal")
	}
}

func TestValidateOptionsRejectsInvalidWatchMode(t *testing.T) {
	err := validateOptions(runOptions{
		changeThreshold: 1,
		signal:          signalResourceVersion,
		watchMode:       watchMode("invalid"),
	})
	if err == nil {
		t.Fatal("validateOptions() should reject invalid watch mode")
	}
}

func TestValidateOptionsRejectsInvalidThreshold(t *testing.T) {
	err := validateOptions(runOptions{
		changeThreshold: 0,
		signal:          signalResourceVersion,
	})
	if err == nil {
		t.Fatal("validateOptions() should reject threshold 0")
	}
}

func TestValidateOptionsRejectsNamespaceConflict(t *testing.T) {
	err := validateOptions(runOptions{
		changeThreshold: 1,
		signal:          signalResourceVersion,
		namespaceScope:  "default",
		allNamespaces:   true,
	})
	if err == nil {
		t.Fatal("validateOptions() should reject namespace conflict")
	}
}

func TestValidateOptionsRejectsInvalidSelectors(t *testing.T) {
	tests := []runOptions{
		{changeThreshold: 1, signal: signalResourceVersion, labelSelector: "app in ("},
		{changeThreshold: 1, signal: signalResourceVersion, fieldSelector: "metadata.name"},
	}

	for _, opts := range tests {
		if err := validateOptions(opts); err == nil {
			t.Fatalf("validateOptions(%#v) should fail", opts)
		}
	}
}

func TestValidateOptionsRejectsInvalidWatchListPageSize(t *testing.T) {
	err := validateOptions(runOptions{
		changeThreshold: 1,
		signal:          signalResourceVersion,
		watchListLimit:  -1,
	})
	if err == nil {
		t.Fatal("validateOptions() should reject negative page size")
	}
}

func TestValidateOptionsRejectsInvalidKubeAPIOverrides(t *testing.T) {
	tests := []runOptions{
		{changeThreshold: 1, signal: signalResourceVersion, kubeAPIQPS: -1},
		{changeThreshold: 1, signal: signalResourceVersion, kubeAPIBurst: -1},
	}

	for _, opts := range tests {
		if err := validateOptions(opts); err == nil {
			t.Fatalf("validateOptions(%#v) should fail", opts)
		}
	}
}

func TestRootCommandFlags(t *testing.T) {
	flags := rootCmd.Flags()

	if flags.Lookup("change-threshold") == nil {
		t.Fatal("missing --change-threshold flag")
	}
	if flags.Lookup("signal") == nil {
		t.Fatal("missing --signal flag")
	}
	if flags.Lookup("watch-mode") == nil {
		t.Fatal("missing --watch-mode flag")
	}
	if flags.Lookup("include-resource") == nil {
		t.Fatal("missing --include-resource flag")
	}
	if flags.Lookup("exclude-resource") == nil {
		t.Fatal("missing --exclude-resource flag")
	}
	if flags.Lookup("namespace-scope") == nil {
		t.Fatal("missing --namespace-scope flag")
	}
	if flags.Lookup("all-namespaces") == nil {
		t.Fatal("missing --all-namespaces flag")
	}
	if flags.Lookup("label-selector") == nil {
		t.Fatal("missing --label-selector flag")
	}
	if flags.Lookup("field-selector") == nil {
		t.Fatal("missing --field-selector flag")
	}
	if flags.Lookup("watch-list-page-size") == nil {
		t.Fatal("missing --watch-list-page-size flag")
	}
	if flags.Lookup("kube-api-qps") == nil {
		t.Fatal("missing --kube-api-qps flag")
	}
	if flags.Lookup("kube-api-burst") == nil {
		t.Fatal("missing --kube-api-burst flag")
	}
	if flags.Lookup("read-only") == nil {
		t.Fatal("missing --read-only flag")
	}

	deprecated := flags.Lookup("generation-changes")
	if deprecated == nil {
		t.Fatal("missing deprecated --generation-changes alias")
	}
	if deprecated.Deprecated == "" {
		t.Fatal("--generation-changes should be marked deprecated")
	}
}

func TestWatchModeSet(t *testing.T) {
	var mode watchMode
	if err := mode.Set("metadata"); err != nil {
		t.Fatalf("Set(metadata) error = %v", err)
	}
	if mode != watchModeMetadata {
		t.Fatalf("mode = %q, want %q", mode, watchModeMetadata)
	}
	if err := mode.Set("unknown"); err == nil {
		t.Fatal("Set(unknown) should fail")
	}
}

func TestSignalSet(t *testing.T) {
	var signal changeSignal
	if err := signal.Set("generation"); err != nil {
		t.Fatalf("Set(generation) error = %v", err)
	}
	if signal != signalGeneration {
		t.Fatalf("signal = %q, want %q", signal, signalGeneration)
	}
	if err := signal.Set("metadata"); err == nil {
		t.Fatal("Set(metadata) should fail")
	}
}
