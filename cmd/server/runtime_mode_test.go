package main

import "testing"

// TestDetermineRuntimeMode pins issue #111: an explicit RUNTIME_MODE wins
// over Lambda auto-detection, and the --mode flag wins over both.
func TestDetermineRuntimeMode(t *testing.T) {
	tests := []struct {
		name        string
		modeFlag    string
		runtimeMode string
		lambdaAPI   string
		want        string
	}{
		{name: "RUNTIME_MODE=http beats Lambda detection", modeFlag: "auto", runtimeMode: "http", lambdaAPI: "127.0.0.1:9001", want: "http"},
		{name: "RUNTIME_MODE=lambda without Lambda env", modeFlag: "auto", runtimeMode: "lambda", want: "lambda"},
		{name: "auto detects Lambda", modeFlag: "auto", runtimeMode: "auto", lambdaAPI: "127.0.0.1:9001", want: "lambda"},
		{name: "unset detects Lambda", modeFlag: "auto", lambdaAPI: "127.0.0.1:9001", want: "lambda"},
		{name: "unset outside Lambda defaults to http", modeFlag: "auto", want: "http"},
		{name: "unrecognized value falls back to detection", modeFlag: "auto", runtimeMode: "bogus", lambdaAPI: "127.0.0.1:9001", want: "lambda"},
		{name: "flag beats RUNTIME_MODE", modeFlag: "lambda", runtimeMode: "http", want: "lambda"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RUNTIME_MODE", tt.runtimeMode)
			t.Setenv("AWS_LAMBDA_RUNTIME_API", tt.lambdaAPI)
			if got := determineRuntimeMode(tt.modeFlag); got != tt.want {
				t.Errorf("determineRuntimeMode(%q) = %q, want %q", tt.modeFlag, got, tt.want)
			}
		})
	}
}
