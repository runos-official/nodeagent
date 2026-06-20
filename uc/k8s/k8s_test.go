package k8s

import (
	"reflect"
	"testing"
)

func TestHelmInstallArgs(t *testing.T) {
	cases := []struct {
		name        string
		releaseName string
		chartRef    string
		namespace   string
		valuesURL   string
		version     string
		want        []string
	}{
		{
			name:        "minimal, no values or version",
			releaseName: "my-release",
			chartRef:    "myrepo/mychart",
			namespace:   "default",
			want: []string{
				"upgrade", "--install", "my-release", "myrepo/mychart",
				"--create-namespace", "--namespace", "default",
			},
		},
		{
			name:        "with values url only",
			releaseName: "my-release",
			chartRef:    "myrepo/mychart",
			namespace:   "kube-system",
			valuesURL:   "https://example.com/values.yaml",
			want: []string{
				"upgrade", "--install", "my-release", "myrepo/mychart",
				"--create-namespace", "--namespace", "kube-system",
				"--values", "https://example.com/values.yaml",
			},
		},
		{
			name:        "with version only",
			releaseName: "my-release",
			chartRef:    "oci://registry.example.com/charts/mychart",
			namespace:   "prod",
			version:     "1.2.3",
			want: []string{
				"upgrade", "--install", "my-release", "oci://registry.example.com/charts/mychart",
				"--create-namespace", "--namespace", "prod",
				"--version", "1.2.3",
			},
		},
		{
			name:        "with values and version, order is values then version",
			releaseName: "rel",
			chartRef:    "repo/chart",
			namespace:   "ns",
			valuesURL:   "https://example.com/v.yaml",
			version:     "9.9.9",
			want: []string{
				"upgrade", "--install", "rel", "repo/chart",
				"--create-namespace", "--namespace", "ns",
				"--values", "https://example.com/v.yaml",
				"--version", "9.9.9",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := helmInstallArgs(tc.releaseName, tc.chartRef, tc.namespace, tc.valuesURL, tc.version)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("helmInstallArgs() =\n  %v\nwant\n  %v", got, tc.want)
			}
		})
	}
}
