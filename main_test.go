package main

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
)

func TestSplitCommaList(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "empty", value: "", want: nil},
		{name: "blank", value: "  ", want: nil},
		{name: "single", value: "IT-Notfall", want: []string{"IT-Notfall"}},
		{name: "multiple with spaces", value: "IT-Notfall, SRM Placeholders", want: []string{"IT-Notfall", "SRM Placeholders"}},
		{name: "empty entries dropped", value: ",IT-Notfall,,", want: []string{"IT-Notfall"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitCommaList(tc.value); !slices.Equal(got, tc.want) {
				t.Errorf("splitCommaList(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestRunDebugModeCompletesAfterOneCollection(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		var output bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))

		if err := runDebugMode(ctx, client, nil, logger); err != nil {
			t.Fatalf("runDebugMode: %v", err)
		}
		if !strings.Contains(output.String(), `msg="cluster capacity calculation"`) {
			t.Fatalf("debug output missing cluster calculation: %s", output.String())
		}

		// Debug mode must stay invisible to the default registry, which is the
		// one the HTTP handler would serve.
		families, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			t.Fatalf("gathering default registry: %v", err)
		}
		for _, family := range families {
			if strings.HasPrefix(family.GetName(), "vcenter_") {
				t.Errorf("debug mode registered %q on the default registry", family.GetName())
			}
		}
	})
}
