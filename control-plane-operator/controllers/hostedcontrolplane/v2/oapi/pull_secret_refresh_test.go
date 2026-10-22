package oapi

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/util"
)

func TestReloadablePullSecrets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		platform   hyperv1.PlatformType
		annotation string
		expected   []string
	}{
		{"When GCP opts in, it should reload only the pull secret", hyperv1.GCPPlatform, "true", []string{"pull-secret"}},
		{"When GCP has no annotation, it should preserve rollout hashing", hyperv1.GCPPlatform, "", nil},
		{"When GCP disables refresh, it should preserve rollout hashing", hyperv1.GCPPlatform, "false", nil},
		{"When AWS has the annotation, it should preserve rollout hashing", hyperv1.AWSPlatform, "true", nil},
		{"When the platform is empty, it should preserve rollout hashing", "", "true", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			hcp := &hyperv1.HostedControlPlane{}
			hcp.Spec.Platform.Type = tc.platform
			if tc.annotation != "" {
				hcp.Annotations = map[string]string{util.GCPPullSecretRefreshAnnotation: tc.annotation}
			}
			g.Expect(reloadablePullSecrets(component.WorkloadContext{HCP: hcp})).To(Equal(tc.expected))
		})
	}
}
