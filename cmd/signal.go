package cmd

import "fmt"

type changeSignal string

const (
	signalResourceVersion changeSignal = "resource-version"
	signalGeneration      changeSignal = "generation"
)

func (s changeSignal) String() string {
	if s == "" {
		return string(signalResourceVersion)
	}
	return string(s)
}

func (s changeSignal) Valid() bool {
	switch s {
	case signalResourceVersion, signalGeneration:
		return true
	default:
		return false
	}
}

func (s *changeSignal) Set(value string) error {
	signal := changeSignal(value)
	if !signal.Valid() {
		return fmt.Errorf("unsupported signal %q", value)
	}
	*s = signal
	return nil
}

func (s changeSignal) Type() string {
	return "signal"
}

func validSignals() []string {
	return []string{string(signalResourceVersion), string(signalGeneration)}
}
