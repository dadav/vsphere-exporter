package main

import (
	"slices"
	"testing"
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
