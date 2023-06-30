/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	sdk "github.com/openshift-online/ocm-sdk-go"
	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	rosacontrolplanev1 "sigs.k8s.io/cluster-api-provider-aws/v2/controlplane/rosa/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/logger"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	capiannotations "sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	rosaControlPlaneKind = "ROSAControlPlane"
	// ROSAControlPlaneFinalizer allows the controller to clean up resources on delete.
	ROSAControlPlaneFinalizer = "rosacontrolplane.controlplane.cluster.x-k8s.io"
)

type ROSAControlPlaneReconciler struct {
	client.Client
	WatchFilterValue string
	WaitInfraPeriod  time.Duration
}

// SetupWithManager is used to setup the controller.
func (r *ROSAControlPlaneReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	log := logger.FromContext(ctx)

	rosaControlPlane := &rosacontrolplanev1.ROSAControlPlane{}
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(rosaControlPlane).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(log.GetLogger(), r.WatchFilterValue)).
		Build(r)

	if err != nil {
		return fmt.Errorf("failed setting up the AWSManagedControlPlane controller manager: %w", err)
	}

	if err = c.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(util.ClusterToInfrastructureMapFunc(ctx, rosaControlPlane.GroupVersionKind(), mgr.GetClient(), &infrav1.ROSACluster{})),
		predicates.ClusterUnpausedAndInfrastructureReady(log.GetLogger()),
	); err != nil {
		return fmt.Errorf("failed adding a watch for ready clusters: %w", err)
	}

	if err = c.Watch(
		&source.Kind{Type: &infrav1.ROSACluster{}},
		handler.EnqueueRequestsFromMapFunc(r.rosaClusterToROSAControlPlane(ctx, log)),
	); err != nil {
		return fmt.Errorf("failed adding a watch for ROSACluster")
	}

	return nil
}

// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinedeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachines;awsmachines/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinetemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmanagedmachinepools;awsmanagedmachinepools/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinepools;awsmachinepools/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=awsmanagedcontrolplanes,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=awsmanagedcontrolplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsclusterroleidentities;awsclusterstaticidentities;awsclustercontrolleridentities,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmanagedclusters;awsmanagedclusters/status,verbs=get;list;watch

// Reconcile will reconcile RosaControlPlane Resources.
func (r *ROSAControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, reterr error) {
	log := logger.FromContext(ctx)

	// Get the control plane instance
	rosaControlPlane := &rosacontrolplanev1.ROSAControlPlane{}
	if err := r.Client.Get(ctx, req.NamespacedName, rosaControlPlane); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Get the cluster
	cluster, err := util.GetOwnerCluster(ctx, r.Client, rosaControlPlane.ObjectMeta)
	if err != nil {
		log.Error(err, "Failed to retrieve owner Cluster from the API Server")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("Cluster Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}

	if capiannotations.IsPaused(cluster, rosaControlPlane) {
		log.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	rosaScope, err := scope.NewROSAControlPlaneScope(scope.ROSAControlPlaneScopeParams{
		Client:         r.Client,
		Cluster:        cluster,
		ControlPlane:   rosaControlPlane,
		ControllerName: strings.ToLower(rosaControlPlaneKind),
	})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to create scope: %w", err)
	}

	// Always close the scope
	defer func() {
		if err := rosaScope.Close(); err != nil && reterr == nil {
			reterr = err
		}
	}()

	if !rosaControlPlane.ObjectMeta.DeletionTimestamp.IsZero() {
		// Handle deletion reconciliation loop.
		return r.reconcileDelete(ctx, rosaScope)
	}

	// Handle normal reconciliation loop.
	return r.reconcileNormal(ctx, rosaScope)
}

func (r *ROSAControlPlaneReconciler) reconcileNormal(ctx context.Context, rosaScope *scope.ROSAControlPlaneScope) (res ctrl.Result, reterr error) {
	rosaScope.Info("Reconciling ROSAControlPlane")

	//if !rosaScope.Cluster.Status.InfrastructureReady {
	//	rosaScope.Info("Cluster infrastructure is not ready yet")
	//	return ctrl.Result{RequeueAfter: r.WaitInfraPeriod}, nil
	//}
	if controllerutil.AddFinalizer(rosaScope.ControlPlane, ROSAControlPlaneFinalizer) {
		if err := rosaScope.PatchObject(); err != nil {
			return ctrl.Result{}, err
		}
	}

	// TODO:
	// - Implement deletion.
	// - Revisit API input.
	// - Suite Machine controller to account for ROSACluster resource.
	// - Explore machine management, e.g capi MachinePool implementation to talk to OCM MachinePool API.

	// Create the cluster:
	clusterBuilder := cmv1.NewCluster().
		Name(rosaScope.ControlPlane.Name).
		MultiAZ(true).
		Product(
			cmv1.NewProduct().
				ID("rosa"),
		).
		Region(
			cmv1.NewCloudRegion().
				ID(*rosaScope.ControlPlane.Spec.Region),
		).
		FIPS(false).
		EtcdEncryption(false).
		DisableUserWorkloadMonitoring(true).
		Version(
			cmv1.NewVersion().
				ID(*rosaScope.ControlPlane.Spec.Version).
				ChannelGroup("stable"),
		).
		ExpirationTimestamp(time.Now().Add(1 * time.Hour)).
		Hypershift(cmv1.NewHypershift().Enabled(true))

	networkBuilder := cmv1.NewNetwork()
	networkBuilder = networkBuilder.Type("OVNKubernetes")
	networkBuilder = networkBuilder.MachineCIDR(*rosaScope.ControlPlane.Spec.MachineCIDR)
	clusterBuilder = clusterBuilder.Network(networkBuilder)

	stsBuilder := cmv1.NewSTS().RoleARN(*rosaScope.ControlPlane.Spec.InstallerRoleARN)
	//stsBuilder = stsBuilder.ExternalID(config.ExternalID)
	stsBuilder = stsBuilder.SupportRoleARN(*rosaScope.ControlPlane.Spec.SupportRoleARN)
	roles := []*cmv1.OperatorIAMRoleBuilder{}
	for _, role := range []struct {
		Name      string
		Namespace string
		RoleARN   string
		Path      string
	}{
		{
			Name:      "cloud-credentials",
			Namespace: "openshift-ingress-operator",
			RoleARN:   rosaScope.ControlPlane.Spec.RolesRef.IngressARN,
		},
		{
			Name:      "installer-cloud-credentials",
			Namespace: "openshift-image-registry",
			RoleARN:   rosaScope.ControlPlane.Spec.RolesRef.ImageRegistryARN,
		},
		{
			Name:      "ebs-cloud-credentials",
			Namespace: "openshift-cluster-csi-drivers",
			RoleARN:   rosaScope.ControlPlane.Spec.RolesRef.StorageARN,
		},
		{
			Name:      "cloud-credentials",
			Namespace: "openshift-cloud-network-config-controller",
			RoleARN:   rosaScope.ControlPlane.Spec.RolesRef.NetworkARN,
		},
		{
			Name:      "kube-controller-manager",
			Namespace: "kube-system",
			RoleARN:   rosaScope.ControlPlane.Spec.RolesRef.KubeCloudControllerARN,
		},
		{
			Name:      "kms-provider",
			Namespace: "kube-system",
			RoleARN:   rosaScope.ControlPlane.Spec.RolesRef.KMSProviderARN,
		},
		{
			Name:      "control-plane-operator",
			Namespace: "kube-system",
			RoleARN:   rosaScope.ControlPlane.Spec.RolesRef.ControlPlaneOperatorARN,
		},
		{
			Name:      "capa-controller-manager",
			Namespace: "kube-system",
			RoleARN:   rosaScope.ControlPlane.Spec.RolesRef.NodePoolManagementARN,
		},
	} {
		roles = append(roles, cmv1.NewOperatorIAMRole().
			Name(role.Name).
			Namespace(role.Namespace).
			RoleARN(role.RoleARN))
	}
	stsBuilder = stsBuilder.OperatorIAMRoles(roles...)

	instanceIAMRolesBuilder := cmv1.NewInstanceIAMRoles()
	instanceIAMRolesBuilder.MasterRoleARN(rosaScope.ControlPlane.Spec.RolesRef.NodePoolManagementARN)
	instanceIAMRolesBuilder.WorkerRoleARN(rosaScope.ControlPlane.Spec.RolesRef.NodePoolManagementARN)
	stsBuilder = stsBuilder.InstanceIAMRoles(instanceIAMRolesBuilder)
	stsBuilder.OidcConfig(cmv1.NewOidcConfig().ID(*rosaScope.ControlPlane.Spec.OIDCID))

	stsBuilder.AutoMode(true)

	awsBuilder := cmv1.NewAWS().
		AccountID(*rosaScope.ControlPlane.Spec.AccountID)
	awsBuilder = awsBuilder.SubnetIDs(rosaScope.ControlPlane.Spec.Subnets...)
	awsBuilder = awsBuilder.STS(stsBuilder)
	clusterBuilder = clusterBuilder.AWS(awsBuilder)

	clusterNodesBuilder := cmv1.NewClusterNodes()
	clusterNodesBuilder = clusterNodesBuilder.AvailabilityZones("us-west-2a")
	clusterBuilder = clusterBuilder.Nodes(clusterNodesBuilder)

	clusterProperties := map[string]string{}
	clusterProperties["rosa_creator_arn"] = *rosaScope.ControlPlane.Spec.CreatorARN

	clusterBuilder = clusterBuilder.Properties(clusterProperties)
	clusterSpec, err := clusterBuilder.Build()
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to create description of cluster: %v", err)
	}

	// Create a logger that has the debug level enabled:
	ocmLogger, err := sdk.NewGoLoggerBuilder().
		Debug(true).
		Build()
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to build logger: %w", err)
	}

	// Create the connection, and remember to close it:
	token := os.Getenv("OCM_TOKEN")
	connection, err := sdk.NewConnectionBuilder().
		Logger(ocmLogger).
		Tokens(token).
		URL("https://api.openshift.com").
		Build()
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to build connection: %w", err)
	}
	defer connection.Close()

	log := logger.FromContext(ctx)
	cluster, err := connection.ClustersMgmt().V1().Clusters().
		Add().
		//Parameter("dryRun", *config.DryRun).
		Body(clusterSpec).
		Send()
	if err != nil {
		log.Info("error", "error", err)
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	clusterObject := cluster.Body()
	log.Info("result", "body", clusterObject)

	return reconcile.Result{}, nil
}

func (r *ROSAControlPlaneReconciler) reconcileDelete(ctx context.Context, rosaScope *scope.ROSAControlPlaneScope) (_ ctrl.Result, reterr error) {
	// log := logger.FromContext(ctx)

	rosaScope.Info("Reconciling ROSAControlPlane delete")

	controlPlane := rosaScope.ControlPlane

	// TODO: Implement OCM Delete

	controllerutil.RemoveFinalizer(controlPlane, ROSAControlPlaneFinalizer)

	return reconcile.Result{}, nil
}

func (r *ROSAControlPlaneReconciler) rosaClusterToROSAControlPlane(ctx context.Context, log *logger.Logger) handler.MapFunc {
	return func(o client.Object) []ctrl.Request {
		rosaCluster, ok := o.(*infrav1.ROSACluster)
		if !ok {
			log.Error(fmt.Errorf("expected a ROSACluster but got a %T", o), "Expected ROSACluster")
			return nil
		}

		if !rosaCluster.ObjectMeta.DeletionTimestamp.IsZero() {
			log.Debug("ROSACluster has a deletion timestamp, skipping mapping")
			return nil
		}

		cluster, err := util.GetOwnerCluster(ctx, r.Client, rosaCluster.ObjectMeta)
		if err != nil {
			log.Error(err, "failed to get owning cluster")
			return nil
		}
		if cluster == nil {
			log.Debug("Owning cluster not set on ROSACluster, skipping mapping")
			return nil
		}

		controlPlaneRef := cluster.Spec.ControlPlaneRef
		if controlPlaneRef == nil || controlPlaneRef.Kind != rosaControlPlaneKind {
			log.Debug("ControlPlaneRef is nil or not ROSAControlPlane, skipping mapping")
			return nil
		}

		return []ctrl.Request{
			{
				NamespacedName: types.NamespacedName{
					Name:      controlPlaneRef.Name,
					Namespace: controlPlaneRef.Namespace,
				},
			},
		}
	}
}
