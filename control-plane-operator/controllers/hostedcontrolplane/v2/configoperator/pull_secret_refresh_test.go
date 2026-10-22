package configoperator

import (
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/testutil"
	"github.com/openshift/hypershift/support/util"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestAdaptDeploymentGCPPullSecretRefresh(t *testing.T) {
	for _, tc := range []struct {
		name       string
		platform   hyperv1.PlatformType
		annotation string
		expected   bool
	}{
		{"When GCP opts in, it should enable HCCO credential refresh", hyperv1.GCPPlatform, "true", true},
		{"When GCP has no annotation, it should preserve existing arguments", hyperv1.GCPPlatform, "", false},
		{"When GCP disables refresh, it should preserve existing arguments", hyperv1.GCPPlatform, "false", false},
		{"When AWS has the annotation, it should preserve existing arguments", hyperv1.AWSPlatform, "true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			hcp := &hyperv1.HostedControlPlane{}
			hcp.Spec.Platform.Type = tc.platform
			hcp.Annotations = map[string]string{util.GCPPullSecretRefreshAnnotation: tc.annotation}
			deployment := &appsv1.Deployment{}
			deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: ComponentName}}
			deployment.Spec.Template.Spec.Volumes = []corev1.Volume{
				{Name: kubeconfigVolumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{}}},
				{Name: rootCAVolumeName, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
				{Name: clusterSignerCAVolumeName, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
			}
			ctx := component.WorkloadContext{Context: t.Context(), HCP: hcp, ReleaseImageProvider: testutil.FakeImageProvider()}
			g.Expect((&hcco{}).adaptDeployment(ctx, deployment)).To(Succeed())
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Command).To(WithTransform(func(args []string) bool {
				for _, arg := range args {
					if arg == "--enable-gcp-pull-secret-refresh=true" {
						return true
					}
				}
				return false
			}, Equal(tc.expected)))
		})
	}
}
