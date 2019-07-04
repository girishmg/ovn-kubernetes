package ovn

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/sirupsen/logrus"
)

// StartClusterMaster runs a subnet IPAM and a controller that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it that will translate
// to creation of a logical switch (done by the node, but could be created here at the master process too)
// Upon deletion of a node, the switch will be deleted
//
// TODO: Verify that the cluster was not already called with a different global subnet
//  If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (oc *MasterController) StartClusterMaster(masterNodeName string, startWatchers bool) error {

	alreadyAllocated := make([]string, 0)
	existingNodes, err := oc.kube.GetNodes()
	if err != nil {
		logrus.Errorf("Error in initializing/fetching subnets: %v", err)
		return err
	}
	for _, node := range existingNodes.Items {
		hostsubnet, ok := node.Annotations[OvnHostSubnet]
		if ok {
			alreadyAllocated = append(alreadyAllocated, hostsubnet)
		}
	}
	masterSubnetAllocatorList := make([]*netutils.SubnetAllocator, 0)
	// NewSubnetAllocator is a subnet IPAM, which takes a CIDR (first argument)
	// and gives out subnets of length 'hostSubnetLength' (second argument)
	// but omitting any that exist in 'subrange' (third argument)
	for _, clusterEntry := range oc.ClusterIPNet {
		subrange := make([]string, 0)
		for _, allocatedRange := range alreadyAllocated {
			firstAddress, _, err := net.ParseCIDR(allocatedRange)
			if err != nil {
				return err
			}
			if clusterEntry.CIDR.Contains(firstAddress) {
				subrange = append(subrange, allocatedRange)
			}
		}
		subnetAllocator, err := netutils.NewSubnetAllocator(clusterEntry.CIDR.String(), 32-clusterEntry.HostSubnetLength, subrange)
		if err != nil {
			return err
		}
		masterSubnetAllocatorList = append(masterSubnetAllocatorList, subnetAllocator)
	}
	oc.masterSubnetAllocatorList = masterSubnetAllocatorList

	if err := oc.SetupMaster(masterNodeName); err != nil {
		logrus.Errorf("Failed to setup master (%v)", err)
		return err
	}

	if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "port_group"); err == nil {
		oc.portGroupSupport = true
	}

	// This flag is set to false when called from the test framework. We cannot run watchers
	// in the case of the test since the code calls several sync*() functions on the
	// resources and that results in several calls to ovn-nbctl commands which are hard to trace
	// and maintain in the test as Fake commands.
	if !startWatchers {
		return nil
	}

	for _, f := range []func() error{oc.WatchPods, oc.WatchServices, oc.WatchEndpoints, oc.WatchNamespaces,
		oc.WatchNetworkPolicy, oc.WatchNodes} {
		if err := f(); err != nil {
			return err
		}
	}

	return nil
}

func setupOVNMaster(nodeName string) error {
	// Configure both server and client of OVN databases, since master uses both
	for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
		if err := auth.SetDBAuth(); err != nil {
			return err
		}
	}
	return nil
}

// SetupMaster creates the central router and load-balancers for the network
func (oc *MasterController) SetupMaster(masterNodeName string) error {
	if err := setupOVNMaster(masterNodeName); err != nil {
		return err
	}

	// Create a single common distributed router for the oc.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "lr-add", OvnClusterRouter,
		"--", "set", "logical_router", OvnClusterRouter, "external_ids:k8s-cluster-router=yes")
	if err != nil {
		logrus.Errorf("Failed to create a single common distributed router for the cluster, "+
			"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Create 2 load-balancers for east-west traffic.  One handles UDP and another handles TCP.
	oc.TCPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes")
	if err != nil {
		logrus.Errorf("Failed to get tcp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}

	if oc.TCPLoadBalancerUUID == "" {
		oc.TCPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes", "protocol=tcp")
		if err != nil {
			logrus.Errorf("Failed to create tcp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	oc.UDPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes")
	if err != nil {
		logrus.Errorf("Failed to get udp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}
	if oc.UDPLoadBalancerUUID == "" {
		oc.UDPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes", "protocol=udp")
		if err != nil {
			logrus.Errorf("Failed to create udp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	// Create a logical switch called "join" that will be used to connect gateway routers to the distributed router.
	// The "join" switch will be allocated IP addresses in the range 100.64.0.0/16.
	const joinSubnet string = "100.64.0.1/16"
	joinIP, joinCIDR, _ := net.ParseCIDR(joinSubnet)
	stdout, stderr, err = util.RunOVNNbctl("--may-exist", "ls-add", "join",
		"--", "set", "logical_switch", "join", fmt.Sprintf("other-config:subnet=%s", joinCIDR.String()),
		"--", "set", "logical_switch", "join", fmt.Sprintf("other-config:exclude_ips=%s", joinIP.String()))
	if err != nil {
		logrus.Errorf("Failed to create logical switch called \"join\", stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Connect the distributed router to "join".
	routerMac, stderr, err := util.RunOVNNbctl("--if-exist", "get", "logical_router_port", "rtoj-"+OvnClusterRouter, "mac")
	if err != nil {
		logrus.Errorf("Failed to get logical router port rtoj-%v, stderr: %q, error: %v", OvnClusterRouter, stderr, err)
		return err
	}
	if routerMac == "" {
		routerMac = util.GenerateMac()
		stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lrp-add", OvnClusterRouter,
			"rtoj-"+OvnClusterRouter, routerMac, joinSubnet)
		if err != nil {
			logrus.Errorf("Failed to add logical router port rtoj-%v, stdout: %q, stderr: %q, error: %v",
				OvnClusterRouter, stdout, stderr, err)
			return err
		}
	}

	// Connect the switch "join" to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", "join", "jtor-"+OvnClusterRouter,
		"--", "set", "logical_switch_port", "jtor-"+OvnClusterRouter, "type=router",
		"options:router-port=rtoj-"+OvnClusterRouter, "addresses="+"\""+routerMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add router-type logical switch port to join, stdout: %q, stderr: %q, error: %v",
			stdout, stderr, err)
		return err
	}

	return nil
}

func parseNodeHostSubnet(node *kapi.Node) (*net.IPNet, error) {
	sub, ok := node.Annotations[OvnHostSubnet]
	if !ok {
		return nil, fmt.Errorf("Error in obtaining host subnet for node %q for deletion", node.Name)
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return nil, fmt.Errorf("Error in parsing hostsubnet - %v", err)
	}

	return subnet, nil
}

func (oc *MasterController) ensureNodeHostSubnet(node *kapi.Node) (*net.IPNet, *netutils.SubnetAllocator, error) {
	// Do not create a subnet if the node already has a subnet
	subnet, _ := parseNodeHostSubnet(node)
	if subnet != nil {
		return subnet, nil, nil
	}

	// Create new subnet
	for _, possibleSubnet := range oc.masterSubnetAllocatorList {
		sn, err := possibleSubnet.GetNetwork()
		if err == netutils.ErrSubnetAllocatorFull {
			// Current subnet exhausted, check next possible subnet
			continue
		} else if err != nil {
			return nil, nil, fmt.Errorf("Error allocating network for node %s: %v", node.Name, err)
		}

		// Success
		logrus.Infof("Allocated node %s HostSubnet %s", node.Name, sn.String())
		return sn, possibleSubnet, nil
	}
	return nil, nil, fmt.Errorf("error allocating network for node %s: No more allocatable ranges", node.Name)
}

func (oc *MasterController) ensureNodeLogicalNetwork(nodeName string, hostsubnet *net.IPNet) error {
	ip := util.NextIP(hostsubnet.IP)
	n, _ := hostsubnet.Mask.Size()
	firstIP := fmt.Sprintf("%s/%d", ip.String(), n)

	nodeLRPMac, stderr, err := util.RunOVNNbctl("--if-exist", "get", "logical_router_port", "rtos-"+nodeName, "mac")
	if err != nil {
		logrus.Errorf("Failed to get logical router port,stderr: %q, error: %v", stderr, err)
		return err
	}
	if nodeLRPMac == "" {
		nodeLRPMac = util.GenerateMac()
	}

	// Create a router port and provide it the first address on the node's host subnet
	_, stderr, err = util.RunOVNNbctl("--may-exist", "lrp-add", OvnClusterRouter, "rtos-"+nodeName, nodeLRPMac, firstIP)
	if err != nil {
		logrus.Errorf("Failed to add logical port to router, stderr: %q, error: %v", stderr, err)
		return err
	}

	// Skip the second address of the LogicalSwitch's subnet since we set it aside for the
	// management port on that node.
	secondIP := util.NextIP(ip)

	// Create a logical switch and set its subnet.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "ls-add", nodeName,
		"--", "set", "logical_switch", nodeName, "other-config:subnet="+hostsubnet.String(),
		"other-config:exclude_ips="+secondIP.String(),
		"external-ids:gateway_ip="+firstIP)
	if err != nil {
		logrus.Errorf("Failed to create a logical switch %v, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	// Connect the switch to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", nodeName, "stor-"+nodeName,
		"--", "set", "logical_switch_port", "stor-"+nodeName, "type=router", "options:router-port=rtos-"+nodeName, "addresses="+"\""+nodeLRPMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Add our cluster TCP and UDP load balancers to the node switch
	if oc.TCPLoadBalancerUUID == "" {
		return fmt.Errorf("TCP cluster load balancer not created")
	}
	stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch", nodeName, "load_balancer="+oc.TCPLoadBalancerUUID)
	if err != nil {
		logrus.Errorf("Failed to set logical switch %v's loadbalancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	if oc.UDPLoadBalancerUUID == "" {
		return fmt.Errorf("UDP cluster load balancer not created")
	}
	stdout, stderr, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "load_balancer", oc.UDPLoadBalancerUUID)
	if err != nil {
		logrus.Errorf("Failed to add logical switch %v's loadbalancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	return nil
}

func (oc *MasterController) addNode(node *kapi.Node) (err error) {
	var hostsubnet *net.IPNet
	var subnetAllocator *netutils.SubnetAllocator
	hostsubnet, subnetAllocator, err = oc.ensureNodeHostSubnet(node)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil && subnetAllocator != nil {
			_ = subnetAllocator.ReleaseNetwork(hostsubnet)
		}
	}()

	// Ensure that the node's logical network has been created
	err = oc.ensureNodeLogicalNetwork(node.Name, hostsubnet)
	if err != nil {
		return err
	}

	// Set the HostSubnet annotation on the node object to signal
	// to nodes that their logical infrastructure is set up and they can
	// proceed with their initialization
	err = oc.kube.SetAnnotationOnNode(node, OvnHostSubnet, hostsubnet.String())
	if err != nil {
		logrus.Errorf("Failed to set node %s host subnet annotation: %v", node.Name, hostsubnet.String())
		return err
	}

	return nil
}

func (oc *MasterController) deleteNodeHostSubnet(nodeName string, subnet *net.IPNet) error {
	for _, possibleSubnet := range oc.masterSubnetAllocatorList {
		if err := possibleSubnet.ReleaseNetwork(subnet); err == nil {
			logrus.Infof("Deleted HostSubnet %v for node %s", subnet, nodeName)
			return nil
		}
	}
	// SubnetAllocator.network is an unexported field so the only way to figure out if a subnet is in a network is to try and delete it
	// if deletion succeeds then stop iterating, if the list is exhausted the node subnet wasn't deleteted return err
	return fmt.Errorf("Error deleting subnet %v for node %q: subnet not found in any CIDR range or already available", subnet, nodeName)
}

func (oc *MasterController) deleteNodeLogicalNetwork(nodeName string) error {
	// Remove the logical switch associated with the node
	if _, stderr, err := util.RunOVNNbctl("--if-exist", "ls-del", nodeName); err != nil {
		return fmt.Errorf("Failed to delete logical switch %s, "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}

	// Remove the patch port that connects distributed router to node's logical switch
	if _, stderr, err := util.RunOVNNbctl("--if-exist", "lrp-del", "rtos-"+nodeName); err != nil {
		return fmt.Errorf("Failed to delete logical router port rtos-%s, "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}

	return nil
}

func (oc *MasterController) deleteNode(nodeName string, nodeSubnet *net.IPNet) error {
	// Clean up as much as we can but don't hard error
	if nodeSubnet != nil {
		if err := oc.deleteNodeHostSubnet(nodeName, nodeSubnet); err != nil {
			logrus.Errorf("Error deleting node %s HostSubnet: %v", nodeName, err)
		}
	}

	if err := oc.deleteNodeLogicalNetwork(nodeName); err != nil {
		logrus.Errorf("Error deleting node %s logical network: %v", nodeName, err)
	}

	if err := util.GatewayCleanup(nodeName); err != nil {
		return fmt.Errorf("Failed to clean up node %s gateway: (%v)", nodeName, err)
	}

	return nil
}

func (oc *MasterController) syncNodes(nodes []interface{}) {
	foundNodes := make(map[string]*kapi.Node)
	for _, tmp := range nodes {
		node, ok := tmp.(*kapi.Node)
		if !ok {
			logrus.Errorf("Spurious object in syncNodes: %v", tmp)
			continue
		}
		foundNodes[node.Name] = node
	}

	// We only deal with cleaning up nodes that shouldn't exist here, since
	// watchNodes() will be called for all existing nodes at startup anyway.
	// Note that this list will include the 'join' cluster switch, which we
	// do not want to delete.
	nodeSwitches, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name,other-config", "find", "logical_switch", "other-config:subnet!=_")
	if err != nil {
		logrus.Errorf("Failed to get node logical switches: stderr: %q, error: %v",
			stderr, err)
		return
	}
	for _, result := range strings.Split(nodeSwitches, "\n\n") {
		// Split result into name and other-config
		items := strings.Split(result, "\n")
		if len(items) != 2 || len(items[0]) == 0 {
			continue
		}
		if items[0] == "join" {
			// Don't delete the cluster switch
			continue
		}
		if _, ok := foundNodes[items[0]]; ok {
			// node still exists, no cleanup to do
			continue
		}

		var subnet *net.IPNet
		if strings.HasPrefix(items[1], "subnet=") {
			subnetStr := strings.TrimPrefix(items[1], "subnet=")
			_, subnet, _ = net.ParseCIDR(subnetStr)
		}

		if err := oc.deleteNode(items[0], subnet); err != nil {
			logrus.Error(err)
		}
	}
}

