package globalps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/openshift/hypershift/control-plane-operator/hostedclusterconfigoperator/controllers/resources/manifests"
	"github.com/openshift/hypershift/support/upsert"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var (
	validAuth = base64.StdEncoding.EncodeToString([]byte("user:pass"))
	oldAuth   = base64.StdEncoding.EncodeToString([]byte("olduser:oldpass"))
)

func TestReconcileGlobalPullSecret(t *testing.T) {
	tests := []struct {
		name                       string
		existingObjects            []client.Object
		nodeObjects                []client.Object
		hcpNamespace               string
		hccoImage                  string
		refreshPullSecret          bool
		expectGlobalSecretExists   bool
		expectOriginalSecretExists bool
		expectDaemonSetExists      bool
		expectServiceAccountExists bool
		expectError                bool
		validateDaemonSet          func(*testing.T, *appsv1.DaemonSet)
		validateGlobalSecret       func(*testing.T, *corev1.Secret)
		validateOriginalSecret     func(*testing.T, *corev1.Secret)
		validateServiceAccount     func(*testing.T, *corev1.ServiceAccount)
	}{
		{
			name:         "When original pull secret exists without additional pull secret, it should create original secret and daemonset without global secret",
			hcpNamespace: "test-hcp",
			hccoImage:    "test-image:latest",
			existingObjects: []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pull-secret",
						Namespace: "test-hcp",
					},
					Data: map[string][]byte{
						corev1.DockerConfigJsonKey: composePullSecretBytes(map[string]string{"registry1.io": validAuth}),
					},
				},
			},
			nodeObjects: []client.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-node-1",
					},
				},
				&capiv1.MachineSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-machineset",
						Namespace: "test-hcp",
					},
					Spec: capiv1.MachineSetSpec{
						Selector: metav1.LabelSelector{
							MatchLabels: map[string]string{"machineset": "test"},
						},
					},
				},
				&capiv1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-machine",
						Namespace: "test-hcp",
						Labels:    map[string]string{"machineset": "test"},
					},
					Status: capiv1.MachineStatus{
						NodeRef: &corev1.ObjectReference{Name: "test-node-1"},
					},
				},
			},
			expectGlobalSecretExists:   false,
			expectOriginalSecretExists: true,
			expectDaemonSetExists:      true,
			expectServiceAccountExists: true,
			expectError:                false,
			validateDaemonSet: func(t *testing.T, ds *appsv1.DaemonSet) {
				g := NewWithT(t)
				g.Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue(globalPSLabelKey, "true"))
				g.Expect(ds.Spec.Template.Spec.Volumes).To(HaveLen(3)) // kubelet-config, dbus, original-pull-secret
				hasGlobalSecret := false
				for _, vol := range ds.Spec.Template.Spec.Volumes {
					if vol.Name == "global-pull-secret" {
						hasGlobalSecret = true
					}
				}
				g.Expect(hasGlobalSecret).To(BeFalse(), "DaemonSet should not have global-pull-secret volume when additional secret doesn't exist")
			},
			validateOriginalSecret: func(t *testing.T, secret *corev1.Secret) {
				g := NewWithT(t)
				g.Expect(secret.Data).To(HaveKey(corev1.DockerConfigJsonKey))
			},
		},
		{
			name:         "When original pull secret and valid additional pull secret exist, it should merge secrets and create daemonset with global secret",
			hcpNamespace: "test-hcp",
			hccoImage:    "test-image:latest",
			existingObjects: []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pull-secret",
						Namespace: "test-hcp",
					},
					Data: map[string][]byte{
						corev1.DockerConfigJsonKey: composePullSecretBytes(map[string]string{"registry1.io": validAuth}),
					},
				},
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "additional-pull-secret",
						Namespace: "kube-system",
					},
					Data: map[string][]byte{
						corev1.DockerConfigJsonKey: composePullSecretBytes(map[string]string{"registry2.io": validAuth}),
					},
				},
			},
			nodeObjects: []client.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-node-1",
					},
				},
				&capiv1.MachineSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-machineset",
						Namespace: "test-hcp",
					},
					Spec: capiv1.MachineSetSpec{
						Selector: metav1.LabelSelector{
							MatchLabels: map[string]string{"machineset": "test"},
						},
					},
				},
				&capiv1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-machine",
						Namespace: "test-hcp",
						Labels:    map[string]string{"machineset": "test"},
					},
					Status: capiv1.MachineStatus{
						NodeRef: &corev1.ObjectReference{Name: "test-node-1"},
					},
				},
			},
			expectGlobalSecretExists:   true,
			expectOriginalSecretExists: true,
			expectDaemonSetExists:      true,
			expectServiceAccountExists: true,
			expectError:                false,
			validateDaemonSet: func(t *testing.T, ds *appsv1.DaemonSet) {
				g := NewWithT(t)
				g.Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue(globalPSLabelKey, "true"))
				g.Expect(ds.Spec.Template.Spec.Volumes).To(HaveLen(4)) // kubelet-config, dbus, original-pull-secret, global-pull-secret
				hasGlobalSecret := false
				for _, vol := range ds.Spec.Template.Spec.Volumes {
					if vol.Name == "global-pull-secret" {
						hasGlobalSecret = true
					}
				}
				g.Expect(hasGlobalSecret).To(BeTrue(), "DaemonSet should have global-pull-secret volume when additional secret exists")
			},
			validateGlobalSecret: func(t *testing.T, secret *corev1.Secret) {
				g := NewWithT(t)
				g.Expect(secret.Data).To(HaveKey(corev1.DockerConfigJsonKey))
				// Verify merged content contains both registries
				var dockerConfigJSON map[string]any
				err := json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &dockerConfigJSON)
				g.Expect(err).NotTo(HaveOccurred())
				auths := dockerConfigJSON["auths"].(map[string]any)
				g.Expect(auths).To(HaveKey("registry1.io"))
				g.Expect(auths).To(HaveKey("registry2.io"))
			},
		},
		{
			name:            "When original pull secret does not exist, it should return error",
			hcpNamespace:    "test-hcp",
			hccoImage:       "test-image:latest",
			existingObjects: []client.Object{
				// No pull secret in control plane namespace
			},
			nodeObjects:                []client.Object{},
			expectGlobalSecretExists:   false,
			expectOriginalSecretExists: false,
			expectDaemonSetExists:      false,
			expectServiceAccountExists: true, // ServiceAccount is created before error occurs
			expectError:                true,
		},
		{
			name:         "When original pull secret exists but missing dockerConfigJson key, it should return error",
			hcpNamespace: "test-hcp",
			hccoImage:    "test-image:latest",
			existingObjects: []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pull-secret",
						Namespace: "test-hcp",
					},
					Data: map[string][]byte{
						"wrong-key": []byte("data"),
					},
				},
			},
			nodeObjects:                []client.Object{},
			expectGlobalSecretExists:   false,
			expectOriginalSecretExists: false,
			expectDaemonSetExists:      false,
			expectServiceAccountExists: true,
			expectError:                true,
		},
		{
			name:              "When refresh is enabled and the original pull secret is empty, it should return error",
			hcpNamespace:      "test-hcp",
			hccoImage:         "quay.io/openshift-release-dev/ocp-v4.0-art-dev:4.22.17",
			refreshPullSecret: true,
			existingObjects: []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: "test-hcp"},
					Data:       map[string][]byte{corev1.DockerConfigJsonKey: {}},
				},
			},
			expectError: true,
		},
		{
			name:         "When additional pull secret has invalid json, it should return error",
			hcpNamespace: "test-hcp",
			hccoImage:    "test-image:latest",
			existingObjects: []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pull-secret",
						Namespace: "test-hcp",
					},
					Data: map[string][]byte{
						corev1.DockerConfigJsonKey: composePullSecretBytes(map[string]string{"registry1.io": validAuth}),
					},
				},
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "additional-pull-secret",
						Namespace: "kube-system",
					},
					Data: map[string][]byte{
						corev1.DockerConfigJsonKey: []byte("invalid json"),
					},
				},
			},
			nodeObjects: []client.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-node-1",
					},
				},
				&capiv1.MachineSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-machineset",
						Namespace: "test-hcp",
					},
					Spec: capiv1.MachineSetSpec{
						Selector: metav1.LabelSelector{
							MatchLabels: map[string]string{"machineset": "test"},
						},
					},
				},
				&capiv1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-machine",
						Namespace: "test-hcp",
						Labels:    map[string]string{"machineset": "test"},
					},
					Status: capiv1.MachineStatus{
						NodeRef: &corev1.ObjectReference{Name: "test-node-1"},
					},
				},
			},
			expectGlobalSecretExists:   false,
			expectOriginalSecretExists: false,
			expectDaemonSetExists:      false,
			expectServiceAccountExists: true,
			expectError:                true,
		},
		{
			name:         "When nodes belong to InPlace NodePools, it should not label nodes",
			hcpNamespace: "test-hcp",
			hccoImage:    "test-image:latest",
			existingObjects: []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pull-secret",
						Namespace: "test-hcp",
					},
					Data: map[string][]byte{
						corev1.DockerConfigJsonKey: composePullSecretBytes(map[string]string{"registry1.io": validAuth}),
					},
				},
			},
			nodeObjects: []client.Object{
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "inplace-node-1",
					},
				},
				&capiv1.MachineSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "inplace-machineset",
						Namespace: "test-hcp",
						Annotations: map[string]string{
							"hypershift.openshift.io/nodePoolTargetConfigVersion": "config-hash-123",
						},
					},
					Spec: capiv1.MachineSetSpec{
						Selector: metav1.LabelSelector{
							MatchLabels: map[string]string{"machineset": "inplace"},
						},
					},
				},
				&capiv1.Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "inplace-machine",
						Namespace: "test-hcp",
						Labels:    map[string]string{"machineset": "inplace"},
					},
					Status: capiv1.MachineStatus{
						NodeRef: &corev1.ObjectReference{Name: "inplace-node-1"},
					},
				},
			},
			expectGlobalSecretExists:   false,
			expectOriginalSecretExists: true,
			expectDaemonSetExists:      true,
			expectServiceAccountExists: true,
			expectError:                false,
			validateDaemonSet: func(t *testing.T, ds *appsv1.DaemonSet) {
				g := NewWithT(t)
				g.Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue(globalPSLabelKey, "true"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			// Create runtime scheme
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = appsv1.AddToScheme(scheme)
			_ = capiv1.AddToScheme(scheme)

			// Create separate clients for different namespaces/purposes
			cpClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.existingObjects...).Build()
			kubeSystemSecretClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.existingObjects...).Build()
			hcUncachedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.existingObjects...).Build()

			// Create reconciler
			reconciler := &Reconciler{
				cpClient:               cpClient,
				kubeSystemSecretClient: kubeSystemSecretClient,
				hcUncachedClient:       hcUncachedClient,
				hcpNamespace:           tt.hcpNamespace,
				hccoImage:              tt.hccoImage,
				refreshPullSecret:      tt.refreshPullSecret,
				CreateOrUpdateProvider: upsert.New(false),
			}

			// Execute reconciliation
			err := reconciler.reconcileGlobalPullSecret(context.Background())

			// Verify error expectation
			if tt.expectError {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())

			// Verify ServiceAccount
			if tt.expectServiceAccountExists {
				sa := &corev1.ServiceAccount{}
				err = hcUncachedClient.Get(context.Background(), client.ObjectKey{
					Name:      "global-pull-secret-syncer",
					Namespace: "kube-system",
				}, sa)
				g.Expect(err).NotTo(HaveOccurred())
				if tt.validateServiceAccount != nil {
					tt.validateServiceAccount(t, sa)
				}
			}

			// Verify global pull secret
			globalSecret := &corev1.Secret{}
			err = kubeSystemSecretClient.Get(context.Background(), client.ObjectKey{
				Name:      "global-pull-secret",
				Namespace: "kube-system",
			}, globalSecret)
			if tt.expectGlobalSecretExists {
				g.Expect(err).NotTo(HaveOccurred())
				if tt.validateGlobalSecret != nil {
					tt.validateGlobalSecret(t, globalSecret)
				}
			} else {
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}

			// Verify original pull secret
			originalSecret := &corev1.Secret{}
			err = kubeSystemSecretClient.Get(context.Background(), client.ObjectKey{
				Name:      "original-pull-secret",
				Namespace: "kube-system",
			}, originalSecret)
			if tt.expectOriginalSecretExists {
				g.Expect(err).NotTo(HaveOccurred())
				if tt.validateOriginalSecret != nil {
					tt.validateOriginalSecret(t, originalSecret)
				}
			} else {
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}

			// Verify DaemonSet
			ds := &appsv1.DaemonSet{}
			err = hcUncachedClient.Get(context.Background(), client.ObjectKey{
				Name:      "global-pull-secret-syncer",
				Namespace: "kube-system",
			}, ds)
			if tt.expectDaemonSetExists {
				g.Expect(err).NotTo(HaveOccurred())
				if tt.validateDaemonSet != nil {
					tt.validateDaemonSet(t, ds)
				}
			} else {
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		})
	}

	renewalTests := []struct {
		name              string
		withAdditional    bool
		rotateAdditional  bool
		refreshPullSecret bool
	}{
		{
			name:              "When original credentials rotate with refresh enabled, it should update the guest Secret without changing the Pod template",
			refreshPullSecret: true,
		},
		{
			name: "When original credentials rotate with refresh disabled, it should update the guest Secret and change the Pod template",
		},
		{
			name:              "When original credentials rotate with an additional Secret and refresh enabled, it should update both guest Secrets without changing the Pod template",
			withAdditional:    true,
			refreshPullSecret: true,
		},
		{
			name:           "When original credentials rotate with an additional Secret and refresh disabled, it should update both guest Secrets and change the Pod template",
			withAdditional: true,
		},
		{
			name:              "When additional credentials rotate with refresh enabled, it should update the merged Secret without changing the Pod template",
			withAdditional:    true,
			rotateAdditional:  true,
			refreshPullSecret: true,
		},
		{
			name:             "When additional credentials rotate with refresh disabled, it should update the merged Secret and change the Pod template",
			withAdditional:   true,
			rotateAdditional: true,
		},
	}
	for _, tt := range renewalTests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()
			scheme := runtime.NewScheme()
			g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
			g.Expect(appsv1.AddToScheme(scheme)).To(Succeed())
			originalAuths := map[string]string{"us-central1-docker.pkg.dev": oldAuth, "quay.io": validAuth}
			additionalAuths := map[string]string{"registry.redhat.io": oldAuth}
			original := manifests.PullSecret("clusters-gar")
			original.Data = map[string][]byte{corev1.DockerConfigJsonKey: composePullSecretBytes(originalAuths)}
			cpClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(original).Build()
			g.Expect(cpClient.Get(ctx, client.ObjectKeyFromObject(original), original)).To(Succeed())
			guestClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			additional := manifests.AdditionalPullSecret()
			if tt.withAdditional {
				additional.Data = map[string][]byte{corev1.DockerConfigJsonKey: composePullSecretBytes(additionalAuths)}
				g.Expect(guestClient.Create(ctx, additional)).To(Succeed())
			}
			r := &Reconciler{
				cpClient:               cpClient,
				kubeSystemSecretClient: guestClient,
				hcUncachedClient:       guestClient,
				hcpNamespace:           original.Namespace,
				hccoImage:              "quay.io/openshift-release-dev/ocp-v4.0-art-dev:4.22.17",
				refreshPullSecret:      tt.refreshPullSecret,
				CreateOrUpdateProvider: upsert.New(false),
			}
			g.Expect(r.reconcileGlobalPullSecret(ctx)).To(Succeed())
			before := manifests.GlobalPullSecretDaemonSet()
			g.Expect(guestClient.Get(ctx, client.ObjectKeyFromObject(before), before)).To(Succeed())
			originalBefore := manifests.OriginalPullSecret()
			g.Expect(guestClient.Get(ctx, client.ObjectKeyFromObject(originalBefore), originalBefore)).To(Succeed())
			g.Expect(originalBefore.Data[corev1.DockerConfigJsonKey]).To(MatchJSON(composePullSecretBytes(originalAuths)))

			if tt.rotateAdditional {
				additionalAuths["registry.redhat.io"] = validAuth
				additional.Data[corev1.DockerConfigJsonKey] = composePullSecretBytes(additionalAuths)
				g.Expect(guestClient.Update(ctx, additional)).To(Succeed())
			} else {
				originalAuths["us-central1-docker.pkg.dev"] = validAuth
				original.Data[corev1.DockerConfigJsonKey] = composePullSecretBytes(originalAuths)
				g.Expect(cpClient.Update(ctx, original)).To(Succeed())
			}
			g.Expect(r.reconcileGlobalPullSecret(ctx)).To(Succeed())
			originalAfter := manifests.OriginalPullSecret()
			g.Expect(guestClient.Get(ctx, client.ObjectKeyFromObject(originalAfter), originalAfter)).To(Succeed())
			g.Expect(originalAfter.Data[corev1.DockerConfigJsonKey]).To(MatchJSON(composePullSecretBytes(originalAuths)))
			if tt.withAdditional {
				mergedAuths := map[string]string{
					"us-central1-docker.pkg.dev": originalAuths["us-central1-docker.pkg.dev"],
					"quay.io":                    validAuth,
					"registry.redhat.io":         additionalAuths["registry.redhat.io"],
				}
				merged := manifests.GlobalPullSecret()
				g.Expect(guestClient.Get(ctx, client.ObjectKeyFromObject(merged), merged)).To(Succeed())
				g.Expect(merged.Data[corev1.DockerConfigJsonKey]).To(MatchJSON(composePullSecretBytes(mergedAuths)))
			}
			after := manifests.GlobalPullSecretDaemonSet()
			g.Expect(guestClient.Get(ctx, client.ObjectKeyFromObject(after), after)).To(Succeed())
			g.Expect(after.Spec.Selector).To(Equal(before.Spec.Selector))
			if tt.refreshPullSecret {
				g.Expect(after.Spec.Template.Labels).NotTo(HaveKey(configSeedLabelKey))
				g.Expect(after.Spec.Template).To(Equal(before.Spec.Template))
			} else {
				g.Expect(after.Spec.Template.Labels[configSeedLabelKey]).NotTo(Equal(before.Spec.Template.Labels[configSeedLabelKey]))
				g.Expect(after.Spec.Template).NotTo(Equal(before.Spec.Template))
			}
		})
	}
}

func TestReconcileDaemonSet(t *testing.T) {
	const hccoImage = "quay.io/openshift-release-dev/ocp-v4.0-art-dev:4.22.17"
	tests := []struct {
		name                   string
		initialRefresh         bool
		refreshPullSecret      bool
		hccoImage              string
		originalPullSecretName string
		globalPullSecretName   string
	}{
		{
			name:              "When refresh is enabled for an existing DaemonSet, it should remove the hash label and preserve its selector",
			refreshPullSecret: true,
		},
		{
			name:           "When refresh is disabled for an existing DaemonSet, it should restore the hash label and preserve its selector",
			initialRefresh: true,
		},
		{
			name:              "When the image changes with refresh enabled, it should change the Pod template",
			initialRefresh:    true,
			refreshPullSecret: true,
			hccoImage:         "quay.io/openshift-release-dev/ocp-v4.0-art-dev:4.22.18",
		},
		{
			name:                   "When the original Secret name changes with refresh enabled, it should change the Pod template",
			initialRefresh:         true,
			refreshPullSecret:      true,
			originalPullSecretName: "replacement-original-pull-secret",
		},
		{
			name:                 "When the global Secret name changes with refresh enabled, it should change the Pod template",
			initialRefresh:       true,
			refreshPullSecret:    true,
			globalPullSecretName: "replacement-global-pull-secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()
			scheme := runtime.NewScheme()
			g.Expect(appsv1.AddToScheme(scheme)).To(Succeed())
			c := fake.NewClientBuilder().WithScheme(scheme).Build()
			provider := upsert.New(false)
			before := manifests.GlobalPullSecretDaemonSet()
			g.Expect(reconcileDaemonSet(ctx, before, "global-pull-secret", "original-pull-secret", "old-hash", tt.initialRefresh, c, provider.CreateOrUpdate, hccoImage)).To(Succeed())
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(before), before)).To(Succeed())
			if tt.hccoImage == "" {
				tt.hccoImage = hccoImage
			}
			if tt.originalPullSecretName == "" {
				tt.originalPullSecretName = "original-pull-secret"
			}
			if tt.globalPullSecretName == "" {
				tt.globalPullSecretName = "global-pull-secret"
			}
			after := manifests.GlobalPullSecretDaemonSet()
			g.Expect(reconcileDaemonSet(ctx, after, tt.globalPullSecretName, tt.originalPullSecretName, "new-hash", tt.refreshPullSecret, c, provider.CreateOrUpdate, tt.hccoImage)).To(Succeed())
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(after), after)).To(Succeed())
			g.Expect(after.Spec.Selector).To(Equal(before.Spec.Selector))
			g.Expect(after.Spec.Selector.MatchLabels).To(Equal(map[string]string{"name": manifests.GlobalPullSecretDSName}))
			g.Expect(after.Spec.Template).NotTo(Equal(before.Spec.Template))
			g.Expect(after.Spec.Template.Spec.NodeSelector).To(Equal(before.Spec.Template.Spec.NodeSelector))
			g.Expect(after.Spec.Template.Spec.Containers[0].Image).To(Equal(tt.hccoImage))
			if tt.refreshPullSecret {
				g.Expect(after.Spec.Template.Labels).NotTo(HaveKey(configSeedLabelKey))
			} else {
				g.Expect(after.Spec.Template.Labels).To(HaveKeyWithValue(configSeedLabelKey, "new-hash"))
			}
			secretNames := map[string]string{}
			for _, volume := range after.Spec.Template.Spec.Volumes {
				if volume.Secret != nil {
					secretNames[volume.Name] = volume.Secret.SecretName
				}
			}
			g.Expect(secretNames).To(Equal(map[string]string{
				"original-pull-secret": tt.originalPullSecretName,
				"global-pull-secret":   tt.globalPullSecretName,
			}))
		})
	}

	t.Run("When the DaemonSet write fails with refresh enabled, it should return the error", func(t *testing.T) {
		g := NewWithT(t)
		writeErr := fmt.Errorf("daemonset update failed")
		createOrUpdate := func(_ context.Context, _ client.Client, _ client.Object, _ controllerutil.MutateFn) (controllerutil.OperationResult, error) {
			return controllerutil.OperationResultNone, writeErr
		}
		err := reconcileDaemonSet(context.Background(), manifests.GlobalPullSecretDaemonSet(), "", "original-pull-secret", "new-hash", true, nil, createOrUpdate, hccoImage)
		g.Expect(err).To(MatchError(ContainSubstring("daemonset update failed")))
		g.Expect(err).To(MatchError(ContainSubstring("failed to create global pull secret daemon set")))
	})
}

func TestValidateAdditionalPullSecret(t *testing.T) {
	tests := []struct {
		name    string
		secret  *corev1.Secret
		wantErr bool
	}{
		{
			name: "valid pull secret",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					corev1.DockerConfigJsonKey: composePullSecretBytes(map[string]string{"quay.io": validAuth}),
				},
			},
			wantErr: false,
		},
		{
			name: "missing docker config key",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"wrong-key": composePullSecretBytes(map[string]string{"quay.io": validAuth}),
				},
			},
			wantErr: true,
		},
		{
			name: "invalid json",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					corev1.DockerConfigJsonKey: []byte(`invalid json`),
				},
			},
			wantErr: true,
		},
		{
			name: "empty auths",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`),
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			_, err := validateAdditionalPullSecret(tt.secret)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
		})
	}
}

func TestMergePullSecrets(t *testing.T) {
	tests := []struct {
		name             string
		originalSecret   []byte
		additionalSecret []byte
		expectedResult   []byte
		wantErr          bool
	}{
		{
			name:             "successful merge with 1 entries",
			originalSecret:   composePullSecretBytes(map[string]string{"registry1": validAuth}),
			additionalSecret: composePullSecretBytes(map[string]string{"registry2": validAuth}),
			expectedResult:   composePullSecretBytes(map[string]string{"registry1": validAuth, "registry2": validAuth}),
			wantErr:          false,
		},
		{
			name:             "successful merge with 2 entries in additional secret",
			originalSecret:   composePullSecretBytes(map[string]string{"registry1": validAuth}),
			additionalSecret: composePullSecretBytes(map[string]string{"registry2": validAuth, "registry3": validAuth}),
			expectedResult:   composePullSecretBytes(map[string]string{"registry1": validAuth, "registry2": validAuth, "registry3": validAuth}),
			wantErr:          false,
		},
		{
			name:             "successful merge with 2 entries in original secret",
			originalSecret:   composePullSecretBytes(map[string]string{"registry1": validAuth, "registry2": validAuth}),
			additionalSecret: composePullSecretBytes(map[string]string{"registry3": validAuth}),
			expectedResult:   composePullSecretBytes(map[string]string{"registry1": validAuth, "registry2": validAuth, "registry3": validAuth}),
			wantErr:          false,
		},
		{
			name:             "conflict resolution - original always wins",
			originalSecret:   composePullSecretBytes(map[string]string{"registry1": oldAuth}),
			additionalSecret: composePullSecretBytes(map[string]string{"registry1": validAuth}),
			expectedResult:   composePullSecretBytes(map[string]string{"registry1": oldAuth}),
			wantErr:          false,
		},
		{
			name:             "precedence test - original always has precedence",
			originalSecret:   composePullSecretBytes(map[string]string{"registry1": oldAuth, "registry2": oldAuth}),
			additionalSecret: composePullSecretBytes(map[string]string{"registry1": validAuth, "registry3": validAuth}),
			expectedResult:   composePullSecretBytes(map[string]string{"registry1": oldAuth, "registry2": oldAuth, "registry3": validAuth}),
			wantErr:          false,
		},
		{
			name:             "multiple conflicts - original always wins",
			originalSecret:   composePullSecretBytes(map[string]string{"registry1": oldAuth, "registry2": oldAuth}),
			additionalSecret: composePullSecretBytes(map[string]string{"registry1": validAuth, "registry2": validAuth, "registry3": validAuth}),
			expectedResult:   composePullSecretBytes(map[string]string{"registry1": oldAuth, "registry2": oldAuth, "registry3": validAuth}),
			wantErr:          false,
		},
		{
			name:             "invalid original secret",
			originalSecret:   []byte(`invalid json`),
			additionalSecret: composePullSecretBytes(map[string]string{"registry1": validAuth}),
			wantErr:          true,
		},
		{
			name:             "invalid additional secret",
			originalSecret:   composePullSecretBytes(map[string]string{"registry1": validAuth}),
			additionalSecret: []byte(`invalid json`),
			wantErr:          true,
		},
		{
			name:             "empty additional secret, invalid JSON",
			originalSecret:   composePullSecretBytes(map[string]string{"registry1": validAuth}),
			additionalSecret: []byte{},
			expectedResult:   composePullSecretBytes(map[string]string{"registry1": validAuth}),
			wantErr:          true,
		},
		{
			name:             "empty additional secret with valid JSON",
			originalSecret:   composePullSecretBytes(map[string]string{"registry1": validAuth, "registry2": validAuth}),
			additionalSecret: []byte(`{"auths":{}}`),
			expectedResult:   composePullSecretBytes(map[string]string{"registry1": validAuth, "registry2": validAuth}),
			wantErr:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			result, err := mergePullSecrets(context.Background(), tt.originalSecret, tt.additionalSecret)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(result).To(Equal(tt.expectedResult))
			}
		})
	}
}

func composePullSecretBytes(auths map[string]string) []byte {
	authsJSON := make(map[string]any)
	authsEntries := make(map[string]any)
	for registry, authEntry := range auths {
		authsEntries[registry] = map[string]any{
			"auth": authEntry,
		}
	}
	authsJSON["auths"] = authsEntries
	authsBytes, err := json.Marshal(authsJSON)
	if err != nil {
		panic(err)
	}
	return authsBytes
}

func TestAdditionalPullSecretExists(t *testing.T) {
	pullSecret := composePullSecretBytes(map[string]string{"quay.io": validAuth})
	tests := []struct {
		name           string
		secretExists   bool
		expectedExists bool
		expectedSecret *corev1.Secret
		objects        []client.Object
	}{
		{
			name:           "secret exists",
			secretExists:   true,
			expectedExists: true,
			expectedSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "additional-pull-secret",
					Namespace: "kube-system",
				},
				Data: map[string][]byte{
					corev1.DockerConfigJsonKey: pullSecret,
				},
			},
			objects: []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "additional-pull-secret",
						Namespace: "kube-system",
					},
					Data: map[string][]byte{
						corev1.DockerConfigJsonKey: pullSecret,
					},
				},
			},
		},
		{
			name:           "secret exists but has no content",
			secretExists:   true,
			expectedExists: true,
			expectedSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "additional-pull-secret",
					Namespace: "kube-system",
				},
				Data: nil,
			},
			objects: []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "additional-pull-secret",
						Namespace: "kube-system",
					},
					Data: nil,
				},
			},
		},
		{
			name:           "secret exists but has incorrect content",
			secretExists:   true,
			expectedExists: true,
			expectedSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "additional-pull-secret",
					Namespace: "kube-system",
				},
				Data: map[string][]byte{
					corev1.DockerConfigJsonKey: []byte(`invalid json content`),
				},
			},
			objects: []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "additional-pull-secret",
						Namespace: "kube-system",
					},
					Data: map[string][]byte{
						corev1.DockerConfigJsonKey: []byte(`invalid json content`),
					},
				},
			},
		},
		{
			name:           "secret does not exist",
			secretExists:   false,
			expectedExists: false,
			expectedSecret: nil,
			objects:        []client.Object{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			fakeClient := fake.NewClientBuilder().WithObjects(tt.objects...).Build()
			exists, secret, err := additionalPullSecretExists(context.Background(), fakeClient)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(exists).To(Equal(tt.expectedExists))

			if tt.expectedSecret != nil {
				g.Expect(secret).NotTo(BeNil())
				g.Expect(secret.Name).To(Equal(tt.expectedSecret.Name))
				g.Expect(secret.Namespace).To(Equal(tt.expectedSecret.Namespace))
				g.Expect(secret.Data).To(Equal(tt.expectedSecret.Data))
			} else {
				g.Expect(secret).To(BeNil())
			}
		})
	}
}
