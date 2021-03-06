package ranchercluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/lasso/pkg/dynamic"
	rancherv1 "github.com/rancher/rancher-operator/pkg/apis/rancher.cattle.io/v1"
	"github.com/rancher/rancher-operator/pkg/clients"
	capicontrollers "github.com/rancher/rancher-operator/pkg/generated/controllers/cluster.x-k8s.io/v1alpha4"
	mgmtcontroller "github.com/rancher/rancher-operator/pkg/generated/controllers/management.cattle.io/v3"
	rocontrollers "github.com/rancher/rancher-operator/pkg/generated/controllers/rancher.cattle.io/v1"
	rkecontroller "github.com/rancher/rancher-operator/pkg/generated/controllers/rke.cattle.io/v1"
	"github.com/rancher/wrangler/pkg/condition"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	corev1 "k8s.io/api/core/v1"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	byNodeInfra = "by-node-infra"
	Provisioned = condition.Cond("Provisioned")
)

type handler struct {
	dynamic           *dynamic.Controller
	dynamicSchema     mgmtcontroller.DynamicSchemaCache
	clusterCache      rocontrollers.ClusterCache
	clusterController rocontrollers.ClusterController
	secretCache       corecontrollers.SecretCache
	secretClient      corecontrollers.SecretClient
	capiClusters      capicontrollers.ClusterCache
	rkeControlPlane   rkecontroller.RKEControlPlaneCache
}

func Register(ctx context.Context, clients *clients.Clients) {
	h := handler{
		dynamic:           clients.Dynamic,
		dynamicSchema:     clients.Management.DynamicSchema().Cache(),
		secretCache:       clients.Core.Secret().Cache(),
		secretClient:      clients.Core.Secret(),
		clusterCache:      clients.Cluster.Cluster().Cache(),
		clusterController: clients.Cluster.Cluster(),
		capiClusters:      clients.CAPI.Cluster().Cache(),
		rkeControlPlane:   clients.RKE.RKEControlPlane().Cache(),
	}

	clients.Dynamic.OnChange(ctx, "rke", matchRKENodeGroup, h.infraWatch)
	clients.Cluster.Cluster().Cache().AddIndexer(byNodeInfra, byNodeInfraIndex)

	rocontrollers.RegisterClusterGeneratingHandler(ctx,
		clients.Cluster.Cluster(),
		clients.Apply.
			WithSetID("rke-cluster").
			WithSetOwnerReference(false, true).
			WithDynamicLookup().
			WithCacheTypes(
				clients.CAPI.Cluster(),
				clients.CAPI.MachineDeployment(),
				clients.RKE.RKECluster(),
				clients.RKE.RKEBootstrapTemplate(),
			),
		"RKECluster",
		"rke-cluster",
		h.OnRancherClusterChange,
		nil)
}

func byNodeInfraIndex(obj *rancherv1.Cluster) ([]string, error) {
	if obj.Status.ClusterName == "" || obj.Spec.RKEConfig == nil {
		return nil, nil
	}

	var result []string
	for _, np := range obj.Spec.RKEConfig.NodePools {
		if np.NodeConfig == nil {
			continue
		}
		result = append(result, toInfraRefKey(*np.NodeConfig, obj.Namespace))
	}

	return result, nil
}

func toInfraRefKey(ref corev1.ObjectReference, namespace string) string {
	if ref.APIVersion == "" {
		ref.APIVersion = "rancher.cattle.io/v1"
	}
	return fmt.Sprintf("%s/%s/%s/%s", ref.APIVersion, ref.Kind, namespace, ref.Name)
}

func matchRKENodeGroup(gvk schema.GroupVersionKind) bool {
	return gvk.Group == "rancher.cattle.io" &&
		strings.HasSuffix(gvk.Kind, "Config")
}

func (h *handler) infraWatch(obj runtime.Object) (runtime.Object, error) {
	if obj == nil {
		return nil, nil
	}

	typeInfo, err := meta.TypeAccessor(obj)
	if err != nil {
		return nil, err
	}

	meta, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}

	indexKey := toInfraRefKey(corev1.ObjectReference{
		Kind:       typeInfo.GetKind(),
		Namespace:  meta.GetNamespace(),
		Name:       meta.GetName(),
		APIVersion: typeInfo.GetAPIVersion(),
	}, meta.GetNamespace())
	clusters, err := h.clusterCache.GetByIndex(byNodeInfra, indexKey)
	if err != nil {
		return nil, err
	}

	for _, cluster := range clusters {
		h.clusterController.Enqueue(cluster.Namespace, cluster.Name)
	}

	return obj, nil
}

func (h *handler) OnRancherClusterChange(obj *rancherv1.Cluster, status rancherv1.ClusterStatus) ([]runtime.Object, rancherv1.ClusterStatus, error) {
	if obj.Spec.RKEConfig == nil || obj.Status.ClusterName == "" {
		return nil, status, nil
	}

	status, err := h.updateClusterProvisioningStatus(obj, status)
	if err != nil {
		return nil, status, err
	}

	objs, err := objects(obj, h.dynamic, h.dynamicSchema)
	return objs, status, err
}

func (h *handler) updateClusterProvisioningStatus(cluster *rancherv1.Cluster, status rancherv1.ClusterStatus) (rancherv1.ClusterStatus, error) {
	capiCluster, err := h.capiClusters.Get(cluster.Namespace, cluster.Name)
	if apierror.IsNotFound(err) {
		return status, nil
	} else if err != nil {
		return status, err
	}

	if capiCluster.Spec.ControlPlaneRef == nil ||
		capiCluster.Spec.ControlPlaneRef.Kind != "RKEControlPlane" {
		return status, nil
	}

	cp, err := h.rkeControlPlane.Get(cluster.Namespace, capiCluster.Spec.ControlPlaneRef.Name)
	if err != nil {
		return status, err
	}

	Provisioned.SetStatus(&status, Provisioned.GetStatus(cp))
	Provisioned.Reason(&status, Provisioned.GetReason(cp))
	Provisioned.Message(&status, Provisioned.GetMessage(cp))
	return status, nil
}
