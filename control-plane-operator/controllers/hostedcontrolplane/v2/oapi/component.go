package oapi

import (
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/common"
	kasv2 "github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/v2/kas"
	component "github.com/openshift/hypershift/support/controlplane-component"
	"github.com/openshift/hypershift/support/util"
)

const (
	ComponentName = "openshift-apiserver"
)

var _ component.ComponentOptions = &openshiftAPIServer{}

type openshiftAPIServer struct {
}

// IsRequestServing implements controlplanecomponent.ComponentOptions.
func (k *openshiftAPIServer) IsRequestServing() bool {
	return false
}

// MultiZoneSpread implements controlplanecomponent.ComponentOptions.
func (k *openshiftAPIServer) MultiZoneSpread() bool {
	return true
}

// NeedsManagementKASAccess implements controlplanecomponent.ComponentOptions.
func (k *openshiftAPIServer) NeedsManagementKASAccess() bool {
	return false
}

func NewComponent() component.ControlPlaneComponent {
	return component.NewDeploymentComponent(ComponentName, &openshiftAPIServer{}).
		WithAdaptFunction(adaptDeployment).
		WithReloadableSecrets(reloadablePullSecrets).
		WithManifestAdapter(
			"config.yaml",
			component.WithAdaptFunction(adaptConfigMap),
		).
		WithManifestAdapter(
			"audit-config.yaml",
			component.WithAdaptFunction(kasv2.AdaptAuditConfig),
		).
		WithManifestAdapter(
			"servicemonitor.yaml",
			component.WithAdaptFunction(adaptServiceMonitor),
		).
		WithManifestAdapter(
			"pdb.yaml",
			component.AdaptPodDisruptionBudget(),
		).
		InjectKonnectivityContainer(component.KonnectivityContainerOptions{
			Mode: component.HTTPS,
		}).
		InjectAvailabilityProberContainer(util.AvailabilityProberOpts{}).
		Build()
}

func reloadablePullSecrets(cpContext component.WorkloadContext) []string {
	if cpContext.HCP.Spec.Platform.Type != hyperv1.GCPPlatform ||
		!util.HasAnnotationWithValue(cpContext.HCP, util.GCPPullSecretRefreshAnnotation, "true") {
		return nil
	}
	// Each ImageStreamImport request creates a new credentials context, which
	// reads the mounted Docker configuration. Token renewal needs no rollout.
	return []string{common.PullSecret(cpContext.HCP.Namespace).Name}
}
