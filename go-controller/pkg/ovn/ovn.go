package ovn

import (
	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
	kapisnetworking "k8s.io/api/networking/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"net"
	"reflect"
	"sync"
)

// MasterController structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints)
type MasterController struct {
	kube         kube.Interface
	watchFactory *factory.WatchFactory

	masterSubnetAllocatorList []*netutils.SubnetAllocator

	TCPLoadBalancerUUID string
	UDPLoadBalancerUUID string

	ClusterIPNet []CIDRNetworkEntry

	gatewayCache map[string]string
	// For TCP and UDP type traffic, cache OVN load-balancers used for the
	// cluster's east-west traffic.
	loadbalancerClusterCache map[string]string

	// For TCP and UDP type traffice, cache OVN load balancer that exists on the
	// default gateway
	loadbalancerGWCache map[string]string

	// A cache of all logical switches seen by the watcher
	logicalSwitchCache map[string]bool

	// A cache of all logical ports seen by the watcher and
	// its corresponding logical switch
	logicalPortCache map[string]string

	// A cache of all logical ports and its corresponding uuids.
	logicalPortUUIDCache map[string]string

	// For each namespace, an address_set that has all the pod IP
	// address in that namespace
	namespaceAddressSet map[string]map[string]bool

	// For each namespace, a lock to protect critical regions
	namespaceMutex map[string]*sync.Mutex

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

// CIDRNetworkEntry is the object that holds the definition for a single network CIDR range
type CIDRNetworkEntry struct {
	CIDR             *net.IPNet
	HostSubnetLength uint32
}

const (
	// OvnHostSubnet is the constant string representing the annotation key
	OvnHostSubnet = "ovn_host_subnet"
	// OvnClusterRouter is the name of the distributed router
	OvnClusterRouter = "ovn_cluster_router"
)

const (
	// TCP is the constant string for the string "TCP"
	TCP = "TCP"

	// UDP is the constant string for the string "UDP"
	UDP = "UDP"
)

// NewMasterController creates a new OVN controller for creating logical network
// infrastructure and policy
func NewMasterController(kubeClient kubernetes.Interface, wf *factory.WatchFactory) *MasterController {
	return &MasterController{
		kube:                     &kube.Kube{KClient: kubeClient},
		watchFactory:             wf,
		logicalSwitchCache:       make(map[string]bool),
		logicalPortCache:         make(map[string]string),
		logicalPortUUIDCache:     make(map[string]string),
		namespaceAddressSet:      make(map[string]map[string]bool),
		namespacePolicies:        make(map[string]map[string]*namespacePolicy),
		namespaceMutex:           make(map[string]*sync.Mutex),
		lspIngressDenyCache:      make(map[string]int),
		lspEgressDenyCache:       make(map[string]int),
		lspMutex:                 &sync.Mutex{},
		lsMutex:                  &sync.Mutex{},
		gatewayCache:             make(map[string]string),
		loadbalancerClusterCache: make(map[string]string),
		loadbalancerGWCache:      make(map[string]string),
	}
}

// WatchPods starts the watching of Pod resource and calls back the appropriate handler logic
func (oc *MasterController) WatchPods() error {
	_, err := oc.watchFactory.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			if pod.Spec.NodeName != "" {
				oc.addLogicalPort(pod)
			}
		},
		UpdateFunc: func(old, newer interface{}) {
			podNew := newer.(*kapi.Pod)
			podOld := old.(*kapi.Pod)
			if podOld.Spec.NodeName == "" && podNew.Spec.NodeName != "" {
				oc.addLogicalPort(podNew)
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			oc.deleteLogicalPort(pod)
		},
	}, oc.syncPods)
	return err
}

// WatchServices starts the watching of Service resource and calls back the
// appropriate handler logic
func (oc *MasterController) WatchServices() error {
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
func (oc *MasterController) WatchEndpoints() error {
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
func (oc *MasterController) WatchNetworkPolicy() error {
	_, err := oc.watchFactory.AddPolicyHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			policy := obj.(*kapisnetworking.NetworkPolicy)
			oc.AddNetworkPolicy(policy)
			return
		},
		UpdateFunc: func(old, newer interface{}) {
			oldPolicy := old.(*kapisnetworking.NetworkPolicy)
			newPolicy := newer.(*kapisnetworking.NetworkPolicy)
			if !reflect.DeepEqual(oldPolicy, newPolicy) {
				oc.deleteNetworkPolicy(oldPolicy)
				oc.AddNetworkPolicy(newPolicy)
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
func (oc *MasterController) WatchNamespaces() error {
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
func (oc *MasterController) WatchNodes() error {
	_, err := oc.watchFactory.AddNodeHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			logrus.Debugf("Added event for Node %q", node.Name)
			err := oc.addNode(node)
			if err != nil {
				logrus.Errorf("error creating subnet for node %s: %v", node.Name, err)
			}
			if config.Gateway.NodeportEnable {
				oc.handleNodePortLB(node)
			}
		},
		UpdateFunc: func(old, new interface{}) {},
		DeleteFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			logrus.Debugf("Delete event for Node %q", node.Name)
			nodeSubnet, _ := parseNodeHostSubnet(node)
			err := oc.deleteNode(node.Name, nodeSubnet)
			if err != nil {
				logrus.Error(err)
			}

			oc.lsMutex.Lock()
			delete(oc.gatewayCache, node.Name)
			delete(oc.logicalSwitchCache, node.Name)
			oc.lsMutex.Unlock()
		},
	}, nil)
	return err
}
