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

func TestValidateOptionsRejectsInvalidThreshold(t *testing.T) {
	err := validateOptions(runOptions{
		changeThreshold: 0,
		signal:          signalResourceVersion,
	})
	if err == nil {
		t.Fatal("validateOptions() should reject threshold 0")
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
	if flags.Lookup("include-resource") == nil {
		t.Fatal("missing --include-resource flag")
	}
	if flags.Lookup("exclude-resource") == nil {
		t.Fatal("missing --exclude-resource flag")
	}
	if flags.Lookup("kube-api-qps") == nil {
		t.Fatal("missing --kube-api-qps flag")
	}
	if flags.Lookup("kube-api-burst") == nil {
		t.Fatal("missing --kube-api-burst flag")
	}

	deprecated := flags.Lookup("generation-changes")
	if deprecated == nil {
		t.Fatal("missing deprecated --generation-changes alias")
	}
	if deprecated.Deprecated == "" {
		t.Fatal("--generation-changes should be marked deprecated")
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
