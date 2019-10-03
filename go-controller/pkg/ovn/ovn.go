package ovn

import (
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"

	knetattachment "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/openshift/origin/pkg/util/netutils"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
	kapisnetworking "k8s.io/api/networking/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type networkAttachmentDefinitionConfig struct {
	isDefault                 bool
	masterSubnetAllocatorList []*netutils.SubnetAllocator
	mtu                       int
	enableGateway             bool
	allocatedNodeSubnets      map[string]string // key is nodeName
	pods                      map[string]bool
}

// Controller structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints)
type Controller struct {
	kube         kube.Interface
	watchFactory *factory.WatchFactory

	netAttchmtDefs map[string]*networkAttachmentDefinitionConfig
	// A mutex for netAttchmtDefs
	netMutex *sync.Mutex

	nodeCache map[string]bool
	// A mutex for nodeCache, held before netMutex
	nodeMutex *sync.Mutex

	// LoadBalance for K8S service access, default netName only
	TCPLoadBalancerUUID string
	UDPLoadBalancerUUID string

	// XXX CATHY gateway IP, key is logical switch name of netPrefix+nodeName
	gatewayCache map[string]string
	// For TCP and UDP type traffic, cache OVN load-balancers used for the
	// cluster's east-west traffic. For K8S service access, default netName
	// only. key is protocol ("TCP" or "UDP")
	loadbalancerClusterCache map[string]string

	// For TCP and UDP type traffice, cache OVN load balancer that exists on the
	// default gateway. For nodePort service access, default netName only
	// key is protocol ("TCP" or "UDP")
	loadbalancerGWCache map[string]string
	defGatewayRouter    string

	// XXX CATHY Existence of node Switch, key is netPrefix+nodeName. A cache of all logical switches seen by the watcher
	logicalSwitchCache map[string]bool

	// XXX CATHY Logical switch of the Pod connect to, key is the logical switch port name of the Pod
	// A cache of all logical ports seen by the watcher and
	// its corresponding logical switch
	logicalPortCache map[string]string

	// XXX CATHY For policy use, UUID of logical port of Pod. default netName only for now.
	// A cache of all logical ports and its corresponding uuids.
	logicalPortUUIDCache map[string]string

	// Policy related. For each namespace, an address_set that has all the pod IP
	// address in that namespace
	namespaceAddressSet map[string]map[string]bool

	// For each namespace, a lock to protect critical regions
	namespaceMutex map[string]*sync.Mutex

	// Need to make calls to namespaceMutex also thread-safe
	namespaceMutexMutex sync.Mutex

	// For each namespace, a map of policy name to 'namespacePolicy'.
	namespacePolicies map[string]map[string]*namespacePolicy

	// Port group for ingress deny rule
	portGroupIngressDeny string

	// Port group for egress deny rule
	portGroupEgressDeny string

	// For each logical port, the number of network policies that want
	// to add a ingress deny rule.
	lspIngressDenyCache map[string]int

	// For each logical port, the number of network policies that want
	// to add a egress deny rule.
	lspEgressDenyCache map[string]int

	// A mutex for lspIngressDenyCache and lspEgressDenyCache
	lspMutex *sync.Mutex

	// A mutex for gatewayCache and logicalSwitchCache which holds
	// logicalSwitch information
	lsMutex *sync.Mutex

	// supports port_group?
	portGroupSupport bool
}

const (
	// TCP is the constant string for the string "TCP"
	TCP = "TCP"

	// UDP is the constant string for the string "UDP"
	UDP = "UDP"
)

// NewOvnController creates a new OVN controller for creating logical network
// infrastructure and policy
func NewOvnController(kubeClient kubernetes.Interface, wf *factory.WatchFactory) *Controller {
	return &Controller{
		kube:                     &kube.Kube{KClient: kubeClient},
		watchFactory:             wf,
		netAttchmtDefs:           make(map[string]*networkAttachmentDefinitionConfig),
		netMutex:                 &sync.Mutex{},
		logicalSwitchCache:       make(map[string]bool),
		logicalPortCache:         make(map[string]string),
		logicalPortUUIDCache:     make(map[string]string),
		namespaceAddressSet:      make(map[string]map[string]bool),
		namespacePolicies:        make(map[string]map[string]*namespacePolicy),
		namespaceMutex:           make(map[string]*sync.Mutex),
		namespaceMutexMutex:      sync.Mutex{},
		lspIngressDenyCache:      make(map[string]int),
		lspEgressDenyCache:       make(map[string]int),
		lspMutex:                 &sync.Mutex{},
		lsMutex:                  &sync.Mutex{},
		gatewayCache:             make(map[string]string),
		loadbalancerClusterCache: make(map[string]string),
		loadbalancerGWCache:      make(map[string]string),
		nodeCache:                make(map[string]bool),
		nodeMutex:                &sync.Mutex{},
	}
}

// Run starts the actual watching.
// watchNetworkAttachmentDefinition needs to be called before WatchNodes() so that the masterSubnetAllocatorList for each
// netName can correctly take into account all the existing subnet range of the existing Nodes
func (oc *Controller) Run() error {
	for _, f := range []func() error{oc.watchNetworkAttachmentDefinition, oc.WatchNodes, oc.WatchPods, oc.WatchServices, oc.WatchEndpoints,
		oc.WatchNamespaces, oc.WatchNetworkPolicy} {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// WatchPods starts the watching of Pod resource and calls back the appropriate handler logic
func (oc *Controller) WatchPods() error {
	_, err := oc.watchFactory.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			if pod.Spec.NodeName != "" {
				logrus.Debugf("CATHY Pod add event for pod %s", pod.Namespace+"/"+pod.Name)
				oc.addPod(pod)
			}
		},
		UpdateFunc: func(old, newer interface{}) {
			podNew := newer.(*kapi.Pod)
			podOld := old.(*kapi.Pod)
			if podOld.Spec.NodeName != podNew.Spec.NodeName {
				logrus.Debugf("CATHY Pod update event for pod %s oldNode %s newNode %s", podNew.Namespace+"/"+podNew.Name,
					podOld.Spec.NodeName, podNew.Spec.NodeName)
				if podOld.Spec.NodeName != "" {
					oc.deletePod(podNew)
				}
				if podNew.Spec.NodeName != "" {
					oc.addPod(podNew)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			logrus.Debugf("CATHY Pod delete event for pod %s", pod.Namespace+"/"+pod.Name)
			oc.deletePod(pod)
		},
	}, oc.syncPods)
	return err
}

// WatchServices starts the watching of Service resource and calls back the
// appropriate handler logic
func (oc *Controller) WatchServices() error {
	_, err := oc.watchFactory.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) {},
		UpdateFunc: func(old, new interface{}) {},
		DeleteFunc: func(obj interface{}) {
			service := obj.(*kapi.Service)
			oc.deleteService(service)
		},
	}, oc.syncServices)
	return err
}

// WatchEndpoints starts the watching of Endpoint resource and calls back the appropriate handler logic
func (oc *Controller) WatchEndpoints() error {
	_, err := oc.watchFactory.AddEndpointsHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			err := oc.AddEndpoints(ep)
			if err != nil {
				logrus.Errorf("Error in adding load balancer: %v", err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			epNew := new.(*kapi.Endpoints)
			epOld := old.(*kapi.Endpoints)
			if reflect.DeepEqual(epNew.Subsets, epOld.Subsets) {
				return
			}
			if len(epNew.Subsets) == 0 {
				err := oc.deleteEndpoints(epNew)
				if err != nil {
					logrus.Errorf("Error in deleting endpoints - %v", err)
				}
			} else {
				err := oc.AddEndpoints(epNew)
				if err != nil {
					logrus.Errorf("Error in modifying endpoints: %v", err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			err := oc.deleteEndpoints(ep)
			if err != nil {
				logrus.Errorf("Error in deleting endpoints - %v", err)
			}
		},
	}, nil)
	return err
}

// WatchNetworkPolicy starts the watching of network policy resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchNetworkPolicy() error {
	_, err := oc.watchFactory.AddPolicyHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			policy := obj.(*kapisnetworking.NetworkPolicy)
			oc.addNetworkPolicy(policy)
			return
		},
		UpdateFunc: func(old, newer interface{}) {
			oldPolicy := old.(*kapisnetworking.NetworkPolicy)
			newPolicy := newer.(*kapisnetworking.NetworkPolicy)
			if !reflect.DeepEqual(oldPolicy, newPolicy) {
				oc.deleteNetworkPolicy(oldPolicy)
				oc.addNetworkPolicy(newPolicy)
			}
			return
		},
		DeleteFunc: func(obj interface{}) {
			policy := obj.(*kapisnetworking.NetworkPolicy)
			oc.deleteNetworkPolicy(policy)
			return
		},
	}, oc.syncNetworkPolicies)
	return err
}

// WatchNamespaces starts the watching of namespace resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchNamespaces() error {
	_, err := oc.watchFactory.AddNamespaceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns := obj.(*kapi.Namespace)
			oc.AddNamespace(ns)
			return
		},
		UpdateFunc: func(old, newer interface{}) {
			// We only use namespace's name and that does not get updated.
			return
		},
		DeleteFunc: func(obj interface{}) {
			ns := obj.(*kapi.Namespace)
			oc.deleteNamespace(ns)
			return
		},
	}, oc.syncNamespaces)
	return err
}

// WatchNodes starts the watching of node resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchNodes() error {
	gatewaysHandled := make(map[string]bool)
	_, err := oc.watchFactory.AddNodeHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			logrus.Debugf("Added event for Node %q", node.Name)
			err := oc.updateNode(nil, node)
			if err != nil {
				logrus.Errorf("error creating subnet for node %s: %v", node.Name, err)
			}
			if !config.Gateway.NodeportEnable {
				return
			}
			gatewaysHandled[node.Name] = oc.handleNodePortLB(node)
		},
		UpdateFunc: func(old, new interface{}) {
			oldNode := old.(*kapi.Node)
			node := new.(*kapi.Node)
			err := oc.updateNode(oldNode, node)
			if err != nil {
				logrus.Errorf("error update Node Management Port for node %s: %v", node.Name, err)
			}
			if !config.Gateway.NodeportEnable {
				return
			}
			if !gatewaysHandled[node.Name] {
				gatewaysHandled[node.Name] = oc.handleNodePortLB(node)
			}
		},
		DeleteFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			logrus.Debugf("Delete event for Node %q. Removing the node from "+
				"various caches", node.Name)

			logrus.Debugf("Delete event for Node %q", node.Name)
			err := oc.deleteNode(node.Name)
			if err != nil {
				logrus.Error(err)
			}
			delete(gatewaysHandled, node.Name)
			oc.lsMutex.Lock()
			if oc.defGatewayRouter == "GR_"+node.Name {
				delete(oc.loadbalancerGWCache, TCP)
				delete(oc.loadbalancerGWCache, UDP)
				oc.defGatewayRouter = ""
				oc.handleExternalIPsLB()
			}
			oc.lsMutex.Unlock()
		},
	}, oc.syncNodes)
	return err
}

func (oc *Controller) addNetworkAttachDefinition(netattachdef *knetattachment.NetworkAttachmentDefinition) error {
	var networkAttDef *networkAttachmentDefinitionConfig

	logrus.Debugf("addNetworkAttachDefinition %s", netattachdef.Name)
	netConf := &ovntypes.NetConf{}
	err := json.Unmarshal([]byte(netattachdef.Spec.Config), &netConf)
	if err != nil {
		logrus.Errorf("AddNetworkAttachDefinition: failed to unmarshal Spec.Config of NetworkAttachmentDefinition %s: %v", netattachdef.Name, err)
		return fmt.Errorf("AddNetworkAttachDefinition: failed to unmarshal Spec.Config of NetworkAttachmentDefinition %s: %v", netattachdef.Name, err)
	}
	// Even if this is the NetworkAttachmentDefinition for the default network, add it to the map, so it is easy to
	// look it up when creating a pod
	if !netConf.NotDefault {
		oc.nodeMutex.Lock()
		oc.netMutex.Lock()
		oc.netAttchmtDefs[netattachdef.Name] = &networkAttachmentDefinitionConfig{
			isDefault:                 true,
			masterSubnetAllocatorList: nil,
			mtu:                       0,
			enableGateway:             false,
			allocatedNodeSubnets:      nil,
			pods:                      nil,
		}
		oc.netMutex.Unlock()
		oc.nodeMutex.Unlock()
		return nil
	}
	// In case name in the json defintion is different from the resource name
	netConf.Name = netattachdef.Name
	networkAttDef, err = oc.SetupMaster(netConf)
	if err != nil {
		logrus.Errorf("AddNetworkAttachDefinition failure: %v", err)
		return err
	}

	subnets := make(map[string]*net.IPNet)
	oc.nodeMutex.Lock()
	oc.netMutex.Lock()
	oc.netAttchmtDefs[netattachdef.Name] = networkAttDef
	for nodeName := range oc.nodeCache {
		subnet, err := oc.getHostSubnet(nodeName, netattachdef.Name, true)
		if err != nil {
			subnets[nodeName] = subnet
		}
	}
	oc.netMutex.Unlock()
	oc.nodeMutex.Unlock()

	for nodeName, subnet := range subnets {
		_ = oc.ensureNodeLogicalNetwork(nodeName, subnet, netattachdef.Name)
	}
	return nil
}

func (oc *Controller) deleteNetworkAttachDefinition(netattachdef *knetattachment.NetworkAttachmentDefinition) {
	logrus.Debugf("deleteNetworkAttachDefinition %s", netattachdef.Name)
	netConf := &ovntypes.NetConf{}
	err := json.Unmarshal([]byte(netattachdef.Spec.Config), &netConf)
	if err != nil {
		logrus.Errorf("deleteNetworkAttachDefinition: failed to unmarshal Spec.Config of NetworkAttachmentDefinition %s: %v", netattachdef.Name, err)
	}

	oc.nodeMutex.Lock()
	oc.netMutex.Lock()
	if len(oc.netAttchmtDefs[netattachdef.Name].pods) != 0 {
		logrus.Errorf("Error: Pods %v still on network %s", oc.netAttchmtDefs[netattachdef.Name].pods, netattachdef.Name)
	}
	delete(oc.netAttchmtDefs, netattachdef.Name)
	nodeNames := oc.nodeCache
	oc.netMutex.Unlock()
	oc.nodeMutex.Unlock()

	// If this is the NetworkAttachmentDefinition for the default network, skip it
	if !netConf.NotDefault {
		return
	}

	oc.deleteMaster(netattachdef.Name)
	for nodeName := range nodeNames {
		if err := oc.deleteNodeLogicalNetwork(nodeName, netattachdef.Name); err != nil {
			logrus.Errorf("Error deleting logical entities for network %s nodeName %s: %v", netattachdef.Name, nodeName, err)
		}

		if err := util.GatewayCleanup(nodeName, netattachdef.Name); err != nil {
			logrus.Errorf("Failed to clean up network %s node %s gateway: (%v)", netattachdef.Name, nodeName, err)
		}
	}
}

// syncNetworkAttachDefinition() delete OVN logical entities of the obsoleted netNames.
func (oc *Controller) syncNetworkAttachDefinition(netattachdefs []interface{}) {

	// Get all the existing non-default netNames
	expectedNetworks := make(map[string]bool)
	for _, netattachdefIntf := range netattachdefs {
		netattachdef, ok := netattachdefIntf.(*knetattachment.NetworkAttachmentDefinition)
		if !ok {
			logrus.Errorf("Spurious object in syncNetworkAttachDefinition: %v", netattachdefIntf)
			continue
		}
		netConf := &ovntypes.NetConf{}
		err := json.Unmarshal([]byte(netattachdef.Spec.Config), &netConf)
		if err != nil {
			logrus.Errorf("Unrecognized Spec.Config of NetworkAttachmentDefinition %s: %v", netattachdef.Name, err)
			continue
		}
		// If this is the NetworkAttachmentDefinition for the default network, skip it
		if !netConf.NotDefault {
			continue
		}
		expectedNetworks[netattachdef.Name] = true
	}

	// Find all the logical node switches for the non-default networks and delete the ones that belong to the
	// obsolete networks
	nodeSwitches, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name,external_ids", "find", "logical_switch", "external_ids:network_name!=_")
	if err != nil {
		logrus.Errorf("Failed to get logical switches with non-default network: stderr: %q, error: %v", stderr, err)
		return
	}
	for _, result := range strings.Split(nodeSwitches, "\n\n") {
		items := strings.Split(result, "\n")
		if len(items) < 2 || len(items[0]) == 0 {
			continue
		}

		netName := util.GetDbValByKey(items[1], "network_name")
		if _, ok := expectedNetworks[netName]; ok {
			// network still exists, no cleanup to do
			continue
		}

		// items[0] is the switch name, which should be prefixed with netName
		if netName == "" || !strings.HasPrefix(items[0], netName) {
			logrus.Warningf("CATHYZ syncNetworkAttachDefinition Unexpected logical switch %s: (%v)", items[0], items)
			continue
		}

		nodeName := strings.TrimPrefix(items[0], netName+"_")
		if nodeName == OvnJoinSwitch {
			// This is the join switch for this network, skip, it will be deleted later below
			continue
		}

		if err := oc.deleteNodeLogicalNetwork(nodeName, netName); err != nil {
			logrus.Errorf("Error deleting node %s logical network: %v", nodeName, err)
		}

		if err = util.GatewayCleanup(nodeName, netName); err != nil {
			logrus.Errorf("Failed to clean up node %s gateway: (%v)", nodeName, err)
		}
	}
	clusterRouters, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name,external_ids", "find", "logical_router", "external_ids:network_name!=_")
	if err != nil {
		logrus.Errorf("Failed to get logical routers with non-default network name: stderr: %q, error: %v",
			stderr, err)
		return
	}
	for _, result := range strings.Split(clusterRouters, "\n\n") {
		items := strings.Split(result, "\n")
		if len(items) < 2 || len(items[0]) == 0 {
			continue
		}

		netName := util.GetDbValByKey(items[1], "network_name")
		if _, ok := expectedNetworks[netName]; ok {
			// network still exists, no cleanup to do
			continue
		}

		// items[0] is the router name, which should be prefixed with netName
		if netName == "" || !strings.HasPrefix(items[0], netName) {
			logrus.Warningf("CATHYZ syncNetworkAttachDefinition Unexpected logical router %s: %v", items[0], result)
			continue
		}

		oc.deleteMaster(netName)
	}
}

func (oc *Controller) watchNetworkAttachmentDefinition() error {
	var err error

	_, err = oc.watchFactory.AddNetworkAttachmentDefinitionHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			netattachdef := obj.(*knetattachment.NetworkAttachmentDefinition)
			logrus.Debugf("netattachdef add event for %q spec %q", netattachdef.Name, netattachdef.Spec.Config)
			err = oc.addNetworkAttachDefinition(netattachdef)
			if err != nil {
				logrus.Errorf("error adding new NetworkAttachmentDefintition %s: %v", netattachdef.Name, err)
			}
		},
		UpdateFunc: func(old, new interface{}) {},
		DeleteFunc: func(obj interface{}) {
			netattachdef := obj.(*knetattachment.NetworkAttachmentDefinition)
			logrus.Debugf("netattachdef delete event for for netattachdef %q", netattachdef.Name, netattachdef.Spec.Config)
			oc.deleteNetworkAttachDefinition(netattachdef)
		},
	}, oc.syncNetworkAttachDefinition)
	return err
}
