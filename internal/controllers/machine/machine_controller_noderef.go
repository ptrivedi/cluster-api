/*
Copyright 2019 The Kubernetes Authors.

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

package machine

import (
	"context"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/api/v1beta1/index"
	"sigs.k8s.io/cluster-api/controllers/noderefutil"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
)

var (
	// ErrNodeNotFound signals that a corev1.Node could not be found for the given provider id.
	ErrNodeNotFound = errors.New("cannot find node with matching ProviderID")
)

func (r *Reconciler) reconcileNode(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Check that the Machine has a valid ProviderID.
	if machine.Spec.ProviderID == nil || *machine.Spec.ProviderID == "" {
		log.Info("Waiting for infrastructure provider to report spec.providerID", machine.Spec.InfrastructureRef.Kind, klog.KRef(machine.Spec.InfrastructureRef.Namespace, machine.Spec.InfrastructureRef.Name))
		conditions.MarkFalse(machine, clusterv1.MachineNodeHealthyCondition, clusterv1.WaitingForNodeRefReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	providerID, err := noderefutil.NewProviderID(*machine.Spec.ProviderID)
	if err != nil {
		return ctrl.Result{}, err
	}

	remoteClient, err := r.Tracker.GetClient(ctx, util.ObjectKey(cluster))
	if err != nil {
		return ctrl.Result{}, err
	}

	// Even if Status.NodeRef exists, continue to do the following checks to make sure Node is healthy
	node, err := r.getNode(ctx, remoteClient, providerID)
	if err != nil {
		if err == ErrNodeNotFound {
			// While a NodeRef is set in the status, failing to get that node means the node is deleted.
			// If Status.NodeRef is not set before, node still can be in the provisioning state.
			if machine.Status.NodeRef != nil {
				conditions.MarkFalse(machine, clusterv1.MachineNodeHealthyCondition, clusterv1.NodeNotFoundReason, clusterv1.ConditionSeverityError, "")
				return ctrl.Result{}, errors.Wrapf(err, "no matching Node for Machine %q in namespace %q", machine.Name, machine.Namespace)
			}
			conditions.MarkFalse(machine, clusterv1.MachineNodeHealthyCondition, clusterv1.NodeProvisioningReason, clusterv1.ConditionSeverityWarning, "")
			// No need to requeue here. Nodes emit an event that triggers reconciliation.
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to retrieve Node by ProviderID")
		r.recorder.Event(machine, corev1.EventTypeWarning, "Failed to retrieve Node by ProviderID", err.Error())
		return ctrl.Result{}, err
	}

	// Set the Machine NodeRef.
	if machine.Status.NodeRef == nil {
		machine.Status.NodeRef = &corev1.ObjectReference{
			Kind:       node.Kind,
			APIVersion: node.APIVersion,
			Name:       node.Name,
			UID:        node.UID,
		}
		log.Info("Infrastructure provider reporting spec.providerID, Kubernetes node is now available", machine.Spec.InfrastructureRef.Kind, klog.KRef(machine.Spec.InfrastructureRef.Namespace, machine.Spec.InfrastructureRef.Name), "providerID", providerID, "node", klog.KRef("", machine.Status.NodeRef.Name))
		r.recorder.Event(machine, corev1.EventTypeNormal, "SuccessfulSetNodeRef", machine.Status.NodeRef.Name)
	}

	// Set the NodeSystemInfo.
	machine.Status.NodeInfo = &node.Status.NodeInfo

	// Reconcile node annotations.
	patchHelper, err := patch.NewHelper(node, remoteClient)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired := map[string]string{
		clusterv1.ClusterNameAnnotation:      machine.Spec.ClusterName,
		clusterv1.ClusterNamespaceAnnotation: machine.GetNamespace(),
		clusterv1.MachineAnnotation:          machine.Name,
	}
	if owner := metav1.GetControllerOfNoCopy(machine); owner != nil {
		desired[clusterv1.OwnerKindAnnotation] = owner.Kind
		desired[clusterv1.OwnerNameAnnotation] = owner.Name
	}
	if annotations.AddAnnotations(node, desired) {
		if err := patchHelper.Patch(ctx, node); err != nil {
			log.V(2).Info("Failed patch node to set annotations", "err", err, "node name", node.Name)
			return ctrl.Result{}, err
		}
	}

	if !r.disableNodeLabelSync {
		options := []client.PatchOption{
			client.FieldOwner("capi-machine"),
			client.ForceOwnership,
		}
		nodePatch := unstructuredNode(node.Name, node.UID, getManagedLabels(machine.Labels))
		if err := remoteClient.Patch(ctx, nodePatch, client.Apply, options...); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to apply patch label to the node")
		}
	}

	// Do the remaining node health checks, then set the node health to true if all checks pass.
	status, message := summarizeNodeConditions(node)
	if status == corev1.ConditionFalse {
		conditions.MarkFalse(machine, clusterv1.MachineNodeHealthyCondition, clusterv1.NodeConditionsFailedReason, clusterv1.ConditionSeverityWarning, message)
		return ctrl.Result{}, nil
	}
	if status == corev1.ConditionUnknown {
		conditions.MarkUnknown(machine, clusterv1.MachineNodeHealthyCondition, clusterv1.NodeConditionsFailedReason, message)
		return ctrl.Result{}, nil
	}

	conditions.MarkTrue(machine, clusterv1.MachineNodeHealthyCondition)
	return ctrl.Result{}, nil
}

// unstructuredNode returns a raw unstructured from Node input.
func unstructuredNode(name string, uid types.UID, labels map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("Node")
	obj.SetName(name)
	obj.SetUID(uid)
	obj.SetLabels(labels)
	return obj
}

// getManagedLabels gets a map[string]string and returns another map[string]string
// filtering out labels not managed by CAPI.
func getManagedLabels(labels map[string]string) map[string]string {
	managedLabels := make(map[string]string)
	for key, value := range labels {
		dnsSubdomainOrName := strings.Split(key, "/")[0]
		if dnsSubdomainOrName == clusterv1.NodeRoleLabelPrefix {
			managedLabels[key] = value
		}
		if dnsSubdomainOrName == clusterv1.NodeRestrictionLabelDomain || strings.HasSuffix(dnsSubdomainOrName, "."+clusterv1.NodeRestrictionLabelDomain) {
			managedLabels[key] = value
		}
		if dnsSubdomainOrName == clusterv1.ManagedNodeLabelDomain || strings.HasSuffix(dnsSubdomainOrName, "."+clusterv1.ManagedNodeLabelDomain) {
			managedLabels[key] = value
		}
	}

	return managedLabels
}

// summarizeNodeConditions summarizes a Node's conditions and returns the summary of condition statuses and concatenate failed condition messages:
// if there is at least 1 semantically-negative condition, summarized status = False;
// if there is at least 1 semantically-positive condition when there is 0 semantically negative condition, summarized status = True;
// if all conditions are unknown,  summarized status = Unknown.
// (semantically true conditions: NodeMemoryPressure/NodeDiskPressure/NodePIDPressure == false or Ready == true.)
func summarizeNodeConditions(node *corev1.Node) (corev1.ConditionStatus, string) {
	semanticallyFalseStatus := 0
	unknownStatus := 0

	message := ""
	for _, condition := range node.Status.Conditions {
		switch condition.Type {
		case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure:
			if condition.Status != corev1.ConditionFalse {
				message += fmt.Sprintf("Node condition %s is %s", condition.Type, condition.Status) + ". "
				if condition.Status == corev1.ConditionUnknown {
					unknownStatus++
					continue
				}
				semanticallyFalseStatus++
			}
		case corev1.NodeReady:
			if condition.Status != corev1.ConditionTrue {
				message += fmt.Sprintf("Node condition %s is %s", condition.Type, condition.Status) + ". "
				if condition.Status == corev1.ConditionUnknown {
					unknownStatus++
					continue
				}
				semanticallyFalseStatus++
			}
		}
	}
	if semanticallyFalseStatus > 0 {
		return corev1.ConditionFalse, message
	}
	if semanticallyFalseStatus+unknownStatus < 4 {
		return corev1.ConditionTrue, message
	}
	return corev1.ConditionUnknown, message
}

func (r *Reconciler) getNode(ctx context.Context, c client.Reader, providerID *noderefutil.ProviderID) (*corev1.Node, error) {
	log := ctrl.LoggerFrom(ctx, "providerID", providerID)
	nodeList := corev1.NodeList{}
	if err := c.List(ctx, &nodeList, client.MatchingFields{index.NodeProviderIDField: providerID.IndexKey()}); err != nil {
		return nil, err
	}
	if len(nodeList.Items) == 0 {
		// If for whatever reason the index isn't registered or available, we fallback to loop over the whole list.
		nl := corev1.NodeList{}
		for {
			if err := c.List(ctx, &nl, client.Continue(nl.Continue)); err != nil {
				return nil, err
			}

			for key, node := range nl.Items {
				nodeProviderID, err := noderefutil.NewProviderID(node.Spec.ProviderID)
				if err != nil {
					log.Error(err, "Failed to parse ProviderID", "Node", klog.KRef("", nl.Items[key].GetName()))
					continue
				}

				if providerID.Equals(nodeProviderID) {
					return &node, nil
				}
			}

			if nl.Continue == "" {
				break
			}
		}

		return nil, ErrNodeNotFound
	}

	if len(nodeList.Items) != 1 {
		return nil, fmt.Errorf("unexpectedly found more than one Node matching the providerID %s", providerID.String())
	}

	return &nodeList.Items[0], nil
}
