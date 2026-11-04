package pluginhost

import "testing"

func TestAPIVersionRange(t *testing.T) {
	cases := []struct {
		rangeExpression string
		accept          []string
		reject          []string
	}{
		{
			rangeExpression: "^1.0",
			accept:          []string{"1.0", "1.0.0", "1.4.0", "1.99.3"},
			reject:          []string{"2.0.0", "0.9.0", "2.0"},
		},
		{
			rangeExpression: "~1.2",
			accept:          []string{"1.2.0", "1.2.9"},
			reject:          []string{"1.3.0", "1.1.0", "2.0.0"},
		},
		{
			rangeExpression: "1.x",
			accept:          []string{"1.0.0", "1.9.9"},
			reject:          []string{"2.0.0", "0.1.0"},
		},
		{
			rangeExpression: "1.2.x",
			accept:          []string{"1.2.0", "1.2.7"},
			reject:          []string{"1.3.0", "1.1.9"},
		},
		{
			rangeExpression: "1.x.x",
			accept:          []string{"1.0.0", "1.9.9"},
			reject:          []string{"2.0.0", "0.9.9"},
		},
		{
			rangeExpression: ">=1.0.0 <2.0.0",
			accept:          []string{"1.0.0", "1.7.2"},
			reject:          []string{"2.0.0", "0.9.9"},
		},
		{
			rangeExpression: "1.2.3",
			accept:          []string{"1.2.3"},
			reject:          []string{"1.2.4", "1.2.2"},
		},
		{
			rangeExpression: "*",
			accept:          []string{"0.0.1", "1.2.3", "9.9.9"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.rangeExpression, func(t *testing.T) {
			for _, version := range tc.accept {
				ok, err := apiVersionSatisfies(tc.rangeExpression, version)
				if err != nil {
					t.Fatalf("%q: %v", version, err)
				}
				if !ok {
					t.Errorf("%q should satisfy %q", version, tc.rangeExpression)
				}
			}
			for _, version := range tc.reject {
				ok, err := apiVersionSatisfies(tc.rangeExpression, version)
				if err != nil {
					t.Fatalf("%q: %v", version, err)
				}
				if ok {
					t.Errorf("%q should not satisfy %q", version, tc.rangeExpression)
				}
			}
		})
	}
}

func TestAPIVersionRangeRejectsGarbage(t *testing.T) {
	for _, expression := range []string{"", "banana", "^", "^^1.0", "1.0.0.0"} {
		if _, err := parseAPIRange(expression); err == nil {
			t.Errorf("%q should not parse", expression)
		}
	}
}

func TestAPIVersionCompatible(t *testing.T) {
	cases := []struct {
		plugin string
		host   string
		want   bool
	}{
		{"1.0", "1.0", true},
		{"1.0.3", "1.0", true},
		{"1.0", "1.4", true},
		{"1.4", "1.0", false}, // plugin is newer than the host
		{"2.0", "1.0", false}, // different major
		{"0.9", "1.0", false},
		{"garbage", "1.0", false},
	}
	for _, tc := range cases {
		if got := apiVersionCompatible(tc.plugin, tc.host); got != tc.want {
			t.Errorf("apiVersionCompatible(%q, %q) = %v, want %v", tc.plugin, tc.host, got, tc.want)
		}
	}
}
