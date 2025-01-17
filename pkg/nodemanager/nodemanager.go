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

package nodemanager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	clientretry "k8s.io/client-go/util/retry"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog"
	kubeletapis "k8s.io/kubernetes/pkg/kubelet/apis"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/api"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
)

// UpdateNodeSpecBackoff defines the backoff for node spec updates.
var UpdateNodeSpecBackoff = wait.Backoff{
	Steps:    20,
	Duration: 50 * time.Millisecond,
	Jitter:   1.0,
}

// NodeProvider defines the interfaces for node provider.
type NodeProvider interface {
	// NodeAddresses returns the addresses of the specified instance.
	NodeAddresses(ctx context.Context, name types.NodeName) ([]v1.NodeAddress, error)
	// InstanceID returns the cloud provider ID of the specified instance.
	InstanceID(ctx context.Context, name types.NodeName) (string, error)
	// InstanceType returns the type of the specified instance.
	InstanceType(ctx context.Context, name types.NodeName) (string, error)
	// GetZone returns the Zone containing the current failure zone and locality region that the program is running in
	GetZone(ctx context.Context) (cloudprovider.Zone, error)
}

// CloudNodeController reconciles node information.
type CloudNodeController struct {
	nodeName     string
	nodeProvider NodeProvider
	nodeInformer coreinformers.NodeInformer
	kubeClient   clientset.Interface
	recorder     record.EventRecorder

	nodeStatusUpdateFrequency time.Duration
}

// NewCloudNodeController creates a CloudNodeController object
func NewCloudNodeController(
	nodeName string,
	nodeInformer coreinformers.NodeInformer,
	kubeClient clientset.Interface,
	nodeProvider NodeProvider,
	nodeStatusUpdateFrequency time.Duration) *CloudNodeController {

	eventBroadcaster := record.NewBroadcaster()
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cloud-node-controller"})
	eventBroadcaster.StartLogging(klog.Infof)
	if kubeClient != nil {
		klog.V(0).Infof("Sending events to api server.")
		eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	} else {
		klog.V(0).Infof("No api server defined - no events will be sent to API server.")
	}

	cnc := &CloudNodeController{
		nodeName:                  nodeName,
		nodeInformer:              nodeInformer,
		kubeClient:                kubeClient,
		recorder:                  recorder,
		nodeProvider:              nodeProvider,
		nodeStatusUpdateFrequency: nodeStatusUpdateFrequency,
	}

	// Use shared informer to listen to add/update of nodes. Note that any nodes
	// that exist before node controller starts will show up in the update method
	cnc.nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    cnc.AddCloudNode,
		UpdateFunc: cnc.UpdateCloudNode,
	})

	return cnc
}

// Run controller updates newly registered nodes with information
// from the cloud provider. This call is blocking so should be called
// via a goroutine
func (cnc *CloudNodeController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	// The following loops run communicate with the APIServer with a worst case complexity
	// of O(num_nodes) per cycle. These functions are justified here because these events fire
	// very infrequently. DO NOT MODIFY this to perform frequent operations.

	// Start a loop to periodically update the node addresses obtained from the cloud
	wait.Until(cnc.UpdateNodeStatus, cnc.nodeStatusUpdateFrequency, stopCh)
}

// UpdateNodeStatus updates the node status, such as node addresses
func (cnc *CloudNodeController) UpdateNodeStatus() {
	nodes, err := cnc.kubeClient.CoreV1().Nodes().List(metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		klog.Errorf("Error monitoring node status: %v", err)
		return
	}

	for i := range nodes.Items {
		cnc.updateNodeAddress(&nodes.Items[i])
	}
}

// UpdateNodeAddress updates the nodeAddress of a single node
func (cnc *CloudNodeController) updateNodeAddress(node *v1.Node) {
	// Do not process nodes that are still tainted
	cloudTaint := getCloudTaint(node.Spec.Taints)
	if cloudTaint != nil {
		klog.V(5).Infof("This node %s is still tainted. Will not process.", node.Name)
		return
	}

	// TODO(feiskyer): Node that isn't present according to the cloud provider shouldn't have its address updated
	nodeAddresses, err := cnc.nodeProvider.NodeAddresses(context.TODO(), types.NodeName(node.Name))
	if err != nil {
		klog.Errorf("%v", err)
		return
	}

	if len(nodeAddresses) == 0 {
		klog.V(5).Infof("Skipping node address update for node %q since cloud provider did not return any", node.Name)
		return
	}

	// Check if a hostname address exists in the cloud provided addresses
	hostnameExists := false
	for i := range nodeAddresses {
		if nodeAddresses[i].Type == v1.NodeHostName {
			hostnameExists = true
		}
	}
	// If hostname was not present in cloud provided addresses, use the hostname
	// from the existing node (populated by kubelet)
	if !hostnameExists {
		for _, addr := range node.Status.Addresses {
			if addr.Type == v1.NodeHostName {
				nodeAddresses = append(nodeAddresses, addr)
			}
		}
	}
	// If nodeIP was suggested by user, ensure that
	// it can be found in the cloud as well (consistent with the behaviour in kubelet)
	if nodeIP, ok := ensureNodeProvidedIPExists(node, nodeAddresses); ok {
		if nodeIP == nil {
			klog.Errorf("Specified Node IP not found in cloudprovider")
			return
		}
	}
	if !nodeAddressesChangeDetected(node.Status.Addresses, nodeAddresses) {
		return
	}
	newNode := node.DeepCopy()
	newNode.Status.Addresses = nodeAddresses
	_, _, err = nodeutil.PatchNodeStatus(cnc.kubeClient.CoreV1(), types.NodeName(node.Name), node, newNode)
	if err != nil {
		klog.Errorf("Error patching node with cloud ip addresses = [%v]", err)
	}
}

// UpdateCloudNode handles node update event.
func (cnc *CloudNodeController) UpdateCloudNode(_, newObj interface{}) {
	node, ok := newObj.(*v1.Node)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", newObj))
		return
	}

	// Skip other nodes other than cnc.nodeName.
	if !strings.EqualFold(cnc.nodeName, node.Name) {
		return
	}

	cloudTaint := getCloudTaint(node.Spec.Taints)
	if cloudTaint == nil {
		// The node has already been initialized so nothing to do.
		return
	}

	cnc.initializeNode(node)
}

// AddCloudNode handles initializing new nodes registered with the cloud taint.
func (cnc *CloudNodeController) AddCloudNode(obj interface{}) {
	node := obj.(*v1.Node)

	// Skip other nodes other than cnc.nodeName.
	if !strings.EqualFold(cnc.nodeName, node.Name) {
		return
	}

	cloudTaint := getCloudTaint(node.Spec.Taints)
	if cloudTaint == nil {
		klog.V(2).Infof("This node %s is registered without the cloud taint. Will not process.", node.Name)
		return
	}

	cnc.initializeNode(node)
}

// This processes nodes that were added into the cluster, and cloud initialize them if appropriate
func (cnc *CloudNodeController) initializeNode(node *v1.Node) {
	err := clientretry.RetryOnConflict(UpdateNodeSpecBackoff, func() error {
		curNode, err := cnc.kubeClient.CoreV1().Nodes().Get(node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		cloudTaint := getCloudTaint(curNode.Spec.Taints)
		if cloudTaint == nil {
			// Node object received from event had the cloud taint but was outdated,
			// the node has actually already been initialized.
			return nil
		}

		if curNode.Spec.ProviderID == "" {
			providerID, err := cnc.nodeProvider.InstanceID(context.TODO(), types.NodeName(curNode.Name))
			if err == nil {
				curNode.Spec.ProviderID = providerID
			} else {
				// we should attempt to set providerID on curNode, but
				// we can continue if we fail since we will attempt to set
				// node addresses given the node name in getNodeAddressesByProviderIDOrName
				klog.Errorf("failed to set node provider id: %v", err)
			}
		}

		nodeAddresses, err := cnc.nodeProvider.NodeAddresses(context.TODO(), types.NodeName(curNode.Name))
		if err != nil {
			return err
		}

		// If user provided an IP address, ensure that IP address is found
		// in the cloud provider before removing the taint on the node
		if nodeIP, ok := ensureNodeProvidedIPExists(curNode, nodeAddresses); ok {
			if nodeIP == nil {
				return errors.New("failed to find kubelet node IP from cloud provider")
			}
		}

		if instanceType, err := cnc.nodeProvider.InstanceType(context.TODO(), types.NodeName(node.Name)); err != nil {
			return err
		} else if instanceType != "" {
			klog.V(2).Infof("Adding node label from cloud provider: %s=%s", v1.LabelInstanceType, instanceType)
			curNode.ObjectMeta.Labels[v1.LabelInstanceType] = instanceType
		}

		// Set zones for the node.
		zone, err := cnc.nodeProvider.GetZone(context.TODO())
		if err != nil {
			return fmt.Errorf("failed to get zone from cloud provider: %v", err)
		}
		if zone.FailureDomain != "" {
			klog.V(2).Infof("Adding node label from cloud provider: %s=%s", v1.LabelZoneFailureDomain, zone.FailureDomain)
			curNode.ObjectMeta.Labels[v1.LabelZoneFailureDomain] = zone.FailureDomain
		}
		if zone.Region != "" {
			klog.V(2).Infof("Adding node label from cloud provider: %s=%s", v1.LabelZoneRegion, zone.Region)
			curNode.ObjectMeta.Labels[v1.LabelZoneRegion] = zone.Region
		}

		curNode.Spec.Taints = excludeCloudTaint(curNode.Spec.Taints)

		_, err = cnc.kubeClient.CoreV1().Nodes().Update(curNode)
		if err != nil {
			return err
		}
		// After adding, call UpdateNodeAddress to set the CloudProvider provided IPAddresses
		// So that users do not see any significant delay in IP addresses being filled into the node
		cnc.updateNodeAddress(curNode)

		klog.Infof("Successfully initialized node %s with cloud provider", node.Name)
		return nil
	})
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
}

func getCloudTaint(taints []v1.Taint) *v1.Taint {
	for _, taint := range taints {
		if taint.Key == schedulerapi.TaintExternalCloudProvider {
			return &taint
		}
	}
	return nil
}

func excludeCloudTaint(taints []v1.Taint) []v1.Taint {
	newTaints := []v1.Taint{}
	for _, taint := range taints {
		if taint.Key == schedulerapi.TaintExternalCloudProvider {
			continue
		}
		newTaints = append(newTaints, taint)
	}
	return newTaints
}

// ensureNodeExistsByProviderID checks if the instance exists by the provider id,
// If provider id in spec is empty it calls instanceId with node name to get provider id
func ensureNodeExistsByProviderID(instances cloudprovider.Instances, node *v1.Node) (bool, error) {
	providerID := node.Spec.ProviderID
	if providerID == "" {
		var err error
		providerID, err = instances.InstanceID(context.TODO(), types.NodeName(node.Name))
		if err != nil {
			if err == cloudprovider.InstanceNotFound {
				return false, nil
			}
			return false, err
		}

		if providerID == "" {
			klog.Warningf("Cannot find valid providerID for node name %q, assuming non existence", node.Name)
			return false, nil
		}
	}

	return instances.InstanceExistsByProviderID(context.TODO(), providerID)
}

func nodeAddressesChangeDetected(addressSet1, addressSet2 []v1.NodeAddress) bool {
	if len(addressSet1) != len(addressSet2) {
		return true
	}
	addressMap1 := map[v1.NodeAddressType]string{}

	for i := range addressSet1 {
		addressMap1[addressSet1[i].Type] = addressSet1[i].Address
	}

	for _, v := range addressSet2 {
		if addressMap1[v.Type] != v.Address {
			return true
		}
	}
	return false
}

func ensureNodeProvidedIPExists(node *v1.Node, nodeAddresses []v1.NodeAddress) (*v1.NodeAddress, bool) {
	var nodeIP *v1.NodeAddress
	nodeIPExists := false
	if providedIP, ok := node.ObjectMeta.Annotations[kubeletapis.AnnotationProvidedIPAddr]; ok {
		nodeIPExists = true
		for i := range nodeAddresses {
			if nodeAddresses[i].Address == providedIP {
				nodeIP = &nodeAddresses[i]
				break
			}
		}
	}
	return nodeIP, nodeIPExists
}
