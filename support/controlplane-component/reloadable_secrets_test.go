package controlplanecomponent

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/api"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestApplyWatchedResourcesAnnotation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		declare      bool
		enabled      bool
		changed      string
		disableAfter bool
		hashChanges  bool
		getError     bool
	}{
		{name: "When a declared reloadable Secret rotates, it should preserve the hash", declare: true, enabled: true, changed: "pull-secret"},
		{name: "When the declaration is disabled, it should hash token changes", declare: true, changed: "pull-secret", hashChanges: true},
		{name: "When another component has no declaration, it should hash the same Secret", enabled: true, changed: "pull-secret", hashChanges: true},
		{name: "When a serving certificate changes, it should change the hash", declare: true, enabled: true, changed: "serving-cert", hashChanges: true},
		{name: "When a mounted ConfigMap changes, it should change the hash", declare: true, enabled: true, changed: "config", hashChanges: true},
		{name: "When refresh is disabled after activation, it should restore the Secret to the hash", declare: true, enabled: true, changed: "pull-secret", disableAfter: true, hashChanges: true},
		{name: "When fetching a watched Secret fails, it should return the error", declare: true, enabled: true, getError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Namespace: "clusters-gar-drejniak", Annotations: map[string]string{}}}
			if tc.enabled {
				hcp.Annotations["refresh"] = "true"
			}
			pullSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: hcp.Namespace}, Data: map[string][]byte{".dockerconfigjson": []byte("old-credential")}}
			servingCert := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "serving-cert", Namespace: hcp.Namespace}, Data: map[string][]byte{"tls.crt": []byte("certificate")}}
			config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: hcp.Namespace}, Data: map[string]string{"config.yaml": "original-config"}}
			builder := fake.NewClientBuilder().WithScheme(api.Scheme).WithObjects(pullSecret, servingCert, config)
			if tc.getError {
				builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
						return errors.New("secret read failed")
					},
				})
			}
			cl := builder.Build()
			cpContext := ControlPlaneContext{Context: t.Context(), Client: cl, HCP: hcp}
			componentBuilder := NewDeploymentComponent("openshift-apiserver", nil)
			if tc.declare {
				componentBuilder.WithReloadableSecrets(func(ctx WorkloadContext) []string {
					if ctx.HCP.Annotations["refresh"] == "true" {
						return []string{"pull-secret"}
					}
					return nil
				})
			}
			workload := componentBuilder.workload
			pod := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
				secretVolume("pull-secret"), secretVolume("serving-cert"), cmVolume("config"),
				projectedVolume("credentials").withSecrets("pull-secret").volume(),
			}}}
			err := workload.applyWatchedResourcesAnnotation(cpContext, &pod)
			if tc.getError {
				g.Expect(err).To(MatchError("secret read failed"))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			before := pod.Annotations["component.hypershift.openshift.io/config-hash"]
			g.Expect(before).NotTo(BeEmpty())
			if tc.disableAfter {
				hcp.Annotations["refresh"] = "false"
			}
			if tc.changed == "config" {
				config.Data["config.yaml"] = "updated-config"
				g.Expect(cl.Update(t.Context(), config)).To(Succeed())
			} else {
				secret := pullSecret
				if tc.changed == "serving-cert" {
					secret = servingCert
				}
				for key := range secret.Data {
					secret.Data[key] = []byte("replacement-data")
				}
				g.Expect(cl.Update(t.Context(), secret)).To(Succeed())
			}
			g.Expect(workload.applyWatchedResourcesAnnotation(cpContext, &pod)).To(Succeed())
			after := pod.Annotations["component.hypershift.openshift.io/config-hash"]
			g.Expect(after != before).To(Equal(tc.hashChanges))
		})
	}
}
