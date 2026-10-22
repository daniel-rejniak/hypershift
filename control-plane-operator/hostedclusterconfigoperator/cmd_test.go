package hostedclusterconfigoperator

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

func TestGlobalPullSecretEnabled(t *testing.T) {
	for _, tc := range []struct {
		name              string
		platform          hyperv1.PlatformType
		enabled, expected bool
	}{
		{"When GCP opts in, it should enable credential propagation", hyperv1.GCPPlatform, true, true},
		{"When GCP does not opt in, it should preserve existing behavior", hyperv1.GCPPlatform, false, false},
		{"When AWS does not opt in, it should keep propagation enabled", hyperv1.AWSPlatform, false, true},
		{"When Azure does not opt in, it should keep propagation enabled", hyperv1.AzurePlatform, false, true},
		{"When another platform opts in, it should keep propagation disabled", hyperv1.NonePlatform, true, false},
		{"When the platform is empty, it should keep propagation disabled", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			NewWithT(t).Expect(globalPullSecretEnabled(string(tc.platform), tc.enabled)).To(Equal(tc.expected))
		})
	}
}
