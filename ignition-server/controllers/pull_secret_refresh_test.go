package controllers

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"github.com/openshift/hypershift/support/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconcileGCPPullSecretRefresh(t *testing.T) {
	for _, tc := range []struct {
		name                                                           string
		enabled, sourceMissing, sourceEmpty, cacheCurrent, expectError bool
		expectCalls                                                    int
	}{
		{name: "When credentials rotate, it should refresh both cached tokens without changing rollout inputs", enabled: true, expectCalls: 1},
		{name: "When credentials match, it should reuse the cached payload", enabled: true, cacheCurrent: true},
		{name: "When refresh is disabled, it should reuse the cache without reading credentials", sourceMissing: true},
		{name: "When the pull secret is missing, it should evict stale credentials and return an error", enabled: true, sourceMissing: true, expectError: true},
		{name: "When the pull secret is empty, it should evict stale credentials and return an error", enabled: true, sourceEmpty: true, expectError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			config, err := util.CompressAndEncode([]byte("bootstrap config"))
			g.Expect(err).ToNot(HaveOccurred())
			token := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-token", Namespace: "clusters-drejniak", Annotations: map[string]string{
					TokenSecretAnnotation: "true", TokenSecretTokenGenerationTime: time.Now().Format(time.RFC3339Nano),
				}},
				Data: map[string][]byte{
					TokenSecretTokenKey: []byte("current-token"), TokenSecretOldTokenKey: []byte("previous-token"),
					TokenSecretReleaseKey: []byte("quay.io/openshift-release-dev/ocp-release:4.22.17-x86_64"),
					TokenSecretConfigKey:  config.Bytes(), TokenSecretPullSecretHashKey: []byte("original-rollout-hash"),
					TokenSecretCloudConfigHashKey: []byte("cloud-config-hash"),
				},
			}
			pullSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pullSecretName, Namespace: token.Namespace},
				Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"us-central1-docker.pkg.dev":{"auth":"rotated-token"}}}`)}}
			if tc.sourceEmpty {
				pullSecret.Data = nil
			}
			objects := []client.Object{token}
			if !tc.sourceMissing {
				objects = append(objects, pullSecret)
			}
			provider := &refreshIgnitionProvider{}
			r := TokenSecretReconciler{Client: fake.NewClientBuilder().WithObjects(objects...).Build(),
				IgnitionProvider: provider, PayloadStore: NewPayloadStore(), RefreshPullSecret: tc.enabled}
			cachedHash := "expired-credentials"
			if tc.cacheCurrent {
				cachedHash = util.HashSimple(pullSecret.Data[corev1.DockerConfigJsonKey])
			}
			for _, id := range []string{"current-token", "previous-token"} {
				r.PayloadStore.Set(id, CacheValue{Payload: []byte("cached"), SecretName: token.Name, CloudConfigHash: "cloud-config-hash", PullSecretHash: cachedHash})
			}
			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(token)})
			if tc.expectError {
				g.Expect(err).To(HaveOccurred())
				for _, id := range []string{"current-token", "previous-token"} {
					_, found := r.PayloadStore.Get(id)
					g.Expect(found).To(BeFalse())
				}
			} else {
				g.Expect(err).ToNot(HaveOccurred())
				if tc.enabled {
					g.Expect(result.RequeueAfter).To(BeNumerically("<=", time.Minute))
				}
				for _, id := range []string{"current-token", "previous-token"} {
					value, found := r.PayloadStore.Get(id)
					g.Expect(found).To(BeTrue())
					if tc.expectCalls > 0 {
						g.Expect(value.PullSecretHash).To(Equal(util.HashSimple(pullSecret.Data[corev1.DockerConfigJsonKey])))
						g.Expect(value.Payload).To(Equal([]byte(provider.pullSecretHash)))
					}
				}
			}
			g.Expect(provider.calls).To(Equal(tc.expectCalls))
			fresh := &corev1.Secret{}
			g.Expect(r.Get(t.Context(), client.ObjectKeyFromObject(token), fresh)).To(Succeed())
			for _, key := range []string{TokenSecretTokenKey, TokenSecretOldTokenKey, TokenSecretPullSecretHashKey, TokenSecretConfigKey, TokenSecretCloudConfigHashKey} {
				g.Expect(fresh.Data[key]).To(Equal(token.Data[key]), "credential refresh must preserve token identity and rollout inputs")
			}
		})
	}
}

type refreshIgnitionProvider struct {
	calls          int
	pullSecretHash string
}

func (p *refreshIgnitionProvider) GetPayload(_ context.Context, _, _, pullSecretHash, _, _, _ string) ([]byte, error) {
	p.calls++
	p.pullSecretHash = pullSecretHash
	return []byte(pullSecretHash), nil
}

func TestTokensForPullSecret(t *testing.T) {
	for _, tc := range []struct {
		name, secret string
		enabled      bool
		expected     int
	}{
		{"When the pull secret changes, it should enqueue only bootstrap tokens in its namespace", pullSecretName, true, 1},
		{"When another secret changes, it should enqueue no tokens", "unrelated", true, 0},
		{"When refresh is disabled, it should enqueue no tokens", pullSecretName, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "worker-token", Namespace: "hcp", Annotations: map[string]string{TokenSecretAnnotation: "true"}}}
			other := token.DeepCopy()
			other.Namespace = "another-hcp"
			unrelated := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "hcp"}}
			r := TokenSecretReconciler{Client: fake.NewClientBuilder().WithObjects(token, other, unrelated).Build(), RefreshPullSecret: tc.enabled}
			requests := r.tokensForPullSecret(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tc.secret, Namespace: "hcp"}})
			g.Expect(requests).To(HaveLen(tc.expected))
			if tc.expected > 0 {
				g.Expect(requests[0].NamespacedName).To(Equal(client.ObjectKeyFromObject(token)))
			}
		})
	}
}
