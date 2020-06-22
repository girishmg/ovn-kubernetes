package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"time"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"

	homaster "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// OvnServiceIdledAt is a constant string representing the Service annotation key
	// whose value indicates the time stamp in RFC3339 format when a Service was idled
	OvnServiceIdledAt = "k8s.ovn.org/idled-at"
)

type ovnkubeMasterLeaderMetrics struct{}

func (ovnkubeMasterLeaderMetrics) On(string) {
	metrics.MetricMasterLeader.Set(1)
}

func (ovnkubeMasterLeaderMetrics) Off(string) {
	metrics.MetricMasterLeader.Set(0)
}

type ovnkubeMasterLeaderMetricsProvider struct{}

func (_ ovnkubeMasterLeaderMetricsProvider) NewLeaderMetric() leaderelection.SwitchMetric {
	return ovnkubeMasterLeaderMetrics{}
}

// Start waits until this process is the leader before starting master functions
func (oc *Controller) Start(kClient kubernetes.Interface, nodeName string) error {
	// Set up leader election process first
	rl, err := resourcelock.New(
		resourcelock.ConfigMapsResourceLock,
		config.Kubernetes.OVNConfigNamespace,
		"ovn-kubernetes-master",
		kClient.CoreV1(),
		nil,
		resourcelock.ResourceLockConfig{Identity: nodeName},
	)
	if err != nil {
		return err
	}

	lec := leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: time.Duration(config.MasterHA.ElectionLeaseDuration) * time.Second,
		RenewDeadline: time.Duration(config.MasterHA.ElectionRenewDeadline) * time.Second,
		RetryPeriod:   time.Duration(config.MasterHA.ElectionRetryPeriod) * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Infof("Won leader election; in active mode")
				// run the cluster controller to init the master
				start := time.Now()
				defer func() {
					end := time.Since(start)
					metrics.MetricMasterReadyDuration.Set(end.Seconds())
				}()
				// run the End-to-end timestamp metric updater only on the
				// active master node.
				metrics.StartE2ETimeStampMetricUpdater(oc.stopChan, oc.ovnNBClient)
				if err := oc.StartClusterMaster(nodeName); err != nil {
					panic(err.Error())
				}
				if err := oc.Run(); err != nil {
					panic(err.Error())
				}
			},
			OnStoppedLeading: func() {
				//This node was leader and it lost the election.
				// Whenever the node transitions from leader to follower,
				// we need to handle the transition properly like clearing
				// the cache. It is better to exit for now.
				// kube will restart and this will become a follower.
				klog.Infof("No longer leader; exiting")
				os.Exit(1)
			},
			OnNewLeader: func(newLeaderName string) {
				if newLeaderName != nodeName {
					klog.Infof("Lost the election to %s; in standby mode", newLeaderName)
				}
			},
		},
	}

	leaderelection.SetProvider(ovnkubeMasterLeaderMetricsProvider{})
	leaderElector, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		return err
	}

	go leaderElector.Run(context.Background())

	return nil
}

// StartClusterMaster runs a subnet IPAM and a controller that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it that will translate
// to creation of a logical switch (done by the node, but could be created here at the master process too)
// Upon deletion of a node, the switch will be deleted
//
// TODO: Verify that the cluster was not already called with a different global subnet
//  If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (oc *Controller) StartClusterMaster(masterNodeName string) error {
	// FIXME DUAL-STACK SUPPORT
	// initialize the subnet required for DNAT and SNAT ip for the shared gateway mode
	_, nodeLocalNatSubnetCIDR, _ := net.ParseCIDR(util.V4NodeLocalNatSubnet)
	oc.nodeLocalNatIPAllocator, _ = ipallocator.NewCIDRRange(nodeLocalNatSubnetCIDR)
	// set aside the first two IPs for the nextHop on the host and for distributed gateway port
	_ = oc.nodeLocalNatIPAllocator.Allocate(net.ParseIP(util.V4NodeLocalNatSubnetNextHop))
	_ = oc.nodeLocalNatIPAllocator.Allocate(net.ParseIP(util.V4NodeLocalDistributedGwPortIP))

	existingNodes, err := oc.kube.GetNodes()
	if err != nil {
		klog.Errorf("Error in initializing/fetching subnets: %v", err)
		return err
	}
	for _, clusterEntry := range config.Default.ClusterSubnets {
		err := oc.masterSubnetAllocator.AddNetworkRange(clusterEntry.CIDR, clusterEntry.HostSubnetLength)
		if err != nil {
			return err
		}
	}
	for _, node := range existingNodes.Items {
		hostSubnets, _ := util.ParseNodeHostSubnetAnnotation(&node)
		for _, hostSubnet := range hostSubnets {
			err := oc.masterSubnetAllocator.MarkAllocatedNetwork(hostSubnet)
			if err != nil {
				utilruntime.HandleError(err)
			}
		}
		nodeLocalNatIPs, _ := util.ParseNodeLocalNatIPAnnotation(&node)
		for _, nodeLocalNatIP := range nodeLocalNatIPs {
			err := oc.nodeLocalNatIPAllocator.Allocate(nodeLocalNatIP)
			if err != nil {
				utilruntime.HandleError(err)
			}
		}
	}

	if _, _, err := util.RunOVNNbctl("--columns=_uuid", "list", "port_group"); err != nil {
		klog.Fatal("OVN version too old; does not support port groups")
	}

	if oc.multicastSupport {
		if _, _, err := util.RunOVNSbctl("--columns=_uuid", "list", "IGMP_Group"); err != nil {
			klog.Warningf("Multicast support enabled, however version of OVN in use does not support IGMP Group. " +
				"Disabling Multicast Support")
			oc.multicastSupport = false
		}
		if !config.IPv4Mode {
			klog.Warningf("Multicast support enabled, but can not be used with single-stack IPv6. Disabling Multicast Support")
			oc.multicastSupport = false
		}
	}

	if err := oc.SetupMaster(masterNodeName); err != nil {
		klog.Errorf("Failed to setup master (%v)", err)
		return err
	}

	if config.HybridOverlay.Enabled {
		factory := oc.watchFactory.GetFactory()
		nodeMaster, err := homaster.NewMaster(
			oc.kube,
			factory.Core().V1().Nodes().Informer(),
			factory.Core().V1().Namespaces().Informer(),
			factory.Core().V1().Pods().Informer(),
			oc.ovnNBClient,
			oc.ovnSBClient,
		)
		if err != nil {
			return fmt.Errorf("failed to set up hybrid overlay master: %v", err)
		}
		go nodeMaster.Run(oc.stopChan)
	}

	return nil
}

// SetupMaster creates the central router and load-balancers for the network
func (oc *Controller) SetupMaster(masterNodeName string) error {
	// Create a single common distributed router for the cluster.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "lr-add", ovnClusterRouter,
		"--", "set", "logical_router", ovnClusterRouter, "external_ids:k8s-cluster-router=yes")
	if err != nil {
		klog.Errorf("Failed to create a single common distributed router for the cluster, "+
			"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	if config.Gateway.Mode == config.GatewayModeShared {
		if err := addDistributedGWPort(); err != nil {
			return err
		}
	}

	// Determine SCTP support
	oc.SCTPSupport, err = util.DetectSCTPSupport()
	if err != nil {
		return err
	}
	if !oc.SCTPSupport {
		klog.Warningf("SCTP unsupported by this version of OVN. Kubernetes service creation with SCTP will not work ")
	} else {
		klog.Info("SCTP support detected in OVN")
	}

	// If supported, enable IGMP relay on the router to forward multicast
	// traffic between nodes.
	if oc.multicastSupport {
		stdout, stderr, err = util.RunOVNNbctl("--", "set", "logical_router",
			ovnClusterRouter, "options:mcast_relay=\"true\"")
		if err != nil {
			klog.Errorf("Failed to enable IGMP relay on the cluster router, "+
				"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}

		// Drop IP multicast globally. Multicast is allowed only if explicitly
		// enabled in a namespace.
		err = createDefaultDenyMulticastPolicy()
		if err != nil {
			klog.Errorf("Failed to create default deny multicast policy, error: %v",
				err)
			return err
		}
	}

	// Create 3 load-balancers for east-west traffic for UDP, TCP, SCTP
	oc.TCPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes")
	if err != nil {
		klog.Errorf("Failed to get tcp load balancer, stderr: %q, error: %v", stderr, err)
		return err
	}

	if oc.TCPLoadBalancerUUID == "" {
		oc.TCPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes", "protocol=tcp")
		if err != nil {
			klog.Errorf("Failed to create tcp load balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	oc.UDPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes")
	if err != nil {
		klog.Errorf("Failed to get udp load balancer, stderr: %q, error: %v", stderr, err)
		return err
	}
	if oc.UDPLoadBalancerUUID == "" {
		oc.UDPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes", "protocol=udp")
		if err != nil {
			klog.Errorf("Failed to create udp load balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	oc.SCTPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-sctp=yes")
	if err != nil {
		klog.Errorf("Failed to get sctp load balancer, stderr: %q, error: %v", stderr, err)
		return err
	}
	if oc.SCTPLoadBalancerUUID == "" && oc.SCTPSupport {
		oc.SCTPLoadBalancerUUID, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-sctp=yes", "protocol=sctp")
		if err != nil {
			klog.Errorf("Failed to create sctp load balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	// Initialize the ovnJoinSwitch switch IP manager
	// The ovnJoinSwitch will be allocated IP addresses in the range 100.64.0.0/16 or fd98::/64.
	oc.joinSwIPManager, err = initJoinLogicalSwitchIPManager()
	if err != nil {
		return err
	}

	// Allocate IPs for logical router port "gwRouterToJoinSwitchPrefix + ovnClusterRouter". This should always
	// allocate the first IPs in the join switch subnets
	gwLRPIPs, err := oc.joinSwIPManager.ensureJoinLRPIPs(ovnClusterRouter)
	if err != nil {
		return fmt.Errorf("failed to allocate join switch IP address connected to %s: %v", ovnClusterRouter, err)
	}

	// Create ovnJoinSwitch that will be used to connect gateway routers to the distributed router.
	_, stderr, err = util.RunOVNNbctl("--may-exist", "ls-add", ovnJoinSwitch)
	if err != nil {
		klog.Errorf("Failed to create logical switch %s, stderr: %q, error: %v", ovnJoinSwitch, stderr, err)
		return err
	}

	// Connect the distributed router to ovnJoinSwitch.
	drSwitchPort := joinSwitchToGwRouterPrefix + ovnClusterRouter
	drRouterPort := gwRouterToJoinSwitchPrefix + ovnClusterRouter
	drLRPMAC := util.IPAddrToHWAddr(gwLRPIPs[0].IP)
	args := []string{
		"--", "--if-exists", "lrp-del", drRouterPort,
		"--", "lrp-add", ovnClusterRouter, drRouterPort, drLRPMAC.String(),
	}
	for _, gwLRPIP := range gwLRPIPs {
		args = append(args, gwLRPIP.String())
	}
	_, stderr, err = util.RunOVNNbctl(args...)
	if err != nil {
		klog.Errorf("Failed to add logical router port %s, stderr: %q, error: %v", drRouterPort, stderr, err)
		return err
	}

	// Connect the switch ovnJoinSwitch to the router.
	_, stderr, err = util.RunOVNNbctl("--may-exist", "lsp-add", ovnJoinSwitch,
		drSwitchPort, "--", "set", "logical_switch_port", drSwitchPort, "type=router",
		"options:router-port="+drRouterPort, "addresses=router")
	if err != nil {
		klog.Errorf("Failed to add router-type logical switch port %s to %s, stderr: %q, error: %v",
			drSwitchPort, ovnJoinSwitch, stderr, err)
		return err
	}

	return nil
}

func (oc *Controller) syncNodeManagementPort(node *kapi.Node, hostSubnets []*net.IPNet) error {
	macAddress, err := util.ParseNodeManagementPortMACAddress(node)
	if err != nil {
		return err
	}

	if hostSubnets == nil {
		hostSubnets, err = util.ParseNodeHostSubnetAnnotation(node)
		if err != nil {
			return err
		}
	}

	var v4Subnet *net.IPNet
	addresses := macAddress.String()
	for _, hostSubnet := range hostSubnets {
		mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
		addresses += " " + mgmtIfAddr.IP.String()

		if err := addAllowACLFromNode(node.Name, mgmtIfAddr.IP); err != nil {
			return err
		}

		if !utilnet.IsIPv6CIDR(hostSubnet) {
			v4Subnet = hostSubnet
		}
	}

	// Create this node's management logical port on the node switch
	stdout, stderr, err := util.RunOVNNbctl(
		"--", "--may-exist", "lsp-add", node.Name, util.K8sPrefix+node.Name,
		"--", "lsp-set-addresses", util.K8sPrefix+node.Name, addresses)
	if err != nil {
		klog.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	if v4Subnet != nil {
		if err := util.UpdateNodeSwitchExcludeIPs(node.Name, v4Subnet); err != nil {
			return err
		}
	}

	return nil
}

func (oc *Controller) syncGatewayLogicalNetwork(node *kapi.Node, l3GatewayConfig *util.L3GatewayConfig, hostSubnets []*net.IPNet) error {
	var err error
	var gwLRPIPs, clusterSubnets []*net.IPNet
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR)
	}

	gwLRPIPs, err = oc.joinSwIPManager.ensureJoinLRPIPs(node.Name)
	if err != nil {
		return fmt.Errorf("failed to allocate join switch port IP address for node %s: %v", node.Name, err)
	}

	drLRPIPs, _ := oc.joinSwIPManager.getJoinLRPCacheIPs(ovnClusterRouter)
	err = gatewayInit(node.Name, clusterSubnets, hostSubnets, l3GatewayConfig, oc.SCTPSupport, gwLRPIPs, drLRPIPs)
	if err != nil {
		return fmt.Errorf("failed to init shared interface gateway: %v", err)
	}

	if l3GatewayConfig.Mode == config.GatewayModeShared {
		// in the case of shared gateway mode, we need to setup
		// 1. two policy based routes to steer traffic to the k8s node IP
		// 	  - from the management port via the node_local_switch's localnet port
		//    - from the hostsubnet via management port
		// 2. a dnat_and_snat nat entry to SNAT the traffic from the management port
		subnets, err := util.ParseNodeHostSubnetAnnotation(node)
		if err != nil {
			return fmt.Errorf("failed to get host subnets for %s: %v", node.Name, err)
		}
		mpMAC, err := util.ParseNodeManagementPortMACAddress(node)
		if err != nil {
			return err
		}
		mpIP := util.GetNodeManagementIfAddr(subnets[0]).IP.String()

		if err := addPolicyBasedRoutes(node.Name, mpIP, l3GatewayConfig.IPAddresses); err != nil {
			return err
		}

		if err := oc.addNodeLocalNatEntries(node, mpMAC.String(), mpIP); err != nil {
			return err
		}
	}

	if l3GatewayConfig.NodePortEnable {
		err = oc.handleNodePortLB(node)
	} else {
		// nodePort disabled, delete gateway load balancers for this node.
		gatewayRouter := "GR_" + node.Name
		for _, proto := range []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolUDP, kapi.ProtocolSCTP} {
			lbUUID, _ := oc.getGatewayLoadBalancer(gatewayRouter, proto)
			if lbUUID != "" {
				_, _, err := util.RunOVNNbctl("--if-exists", "destroy", "load_balancer", lbUUID)
				if err != nil {
					klog.Errorf("Failed to destroy %s load balancer for gateway %s: %v", proto, gatewayRouter, err)
				}
			}
		}
	}

	return err
}

func (oc *Controller) ensureNodeLogicalNetwork(nodeName string, hostSubnets []*net.IPNet) error {
	// logical router port MAC is based on IPv4 subnet if there is one, else IPv6
	var nodeLRPMAC net.HardwareAddr
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		nodeLRPMAC = util.IPAddrToHWAddr(gwIfAddr.IP)
		if !utilnet.IsIPv6CIDR(hostSubnet) {
			break
		}
	}

	lrpArgs := []string{
		"--if-exists", "lrp-del", routerToSwitchPrefix + nodeName,
		"--", "lrp-add", ovnClusterRouter, routerToSwitchPrefix + nodeName,
		nodeLRPMAC.String(),
	}

	lsArgs := []string{
		"--may-exist",
		"ls-add", nodeName,
		"--", "set", "logical_switch", nodeName,
	}

	var v4Gateway net.IP
	for _, hostSubnet := range hostSubnets {
		gwIfAddr := util.GetNodeGatewayIfAddr(hostSubnet)
		lrpArgs = append(lrpArgs, gwIfAddr.String())

		if utilnet.IsIPv6CIDR(hostSubnet) {
			lsArgs = append(lsArgs,
				"other-config:ipv6_prefix="+hostSubnet.IP.String(),
			)
		} else {
			v4Gateway = gwIfAddr.IP

			mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
			excludeIPs := mgmtIfAddr.IP.String()
			if config.HybridOverlay.Enabled {
				hybridOverlayIfAddr := util.GetNodeHybridOverlayIfAddr(hostSubnet)
				excludeIPs += ".." + hybridOverlayIfAddr.IP.String()
			}
			lsArgs = append(lsArgs,
				"other-config:subnet="+hostSubnet.String(),
				"other-config:exclude_ips="+excludeIPs,
			)
		}
	}

	// Create a router port and provide it the first address on the node's host subnet
	_, stderr, err := util.RunOVNNbctl(lrpArgs...)
	if err != nil {
		klog.Errorf("Failed to add logical port to router, stderr: %q, error: %v", stderr, err)
		return err
	}

	// Create a logical switch and set its subnet.
	stdout, stderr, err := util.RunOVNNbctl(lsArgs...)
	if err != nil {
		klog.Errorf("Failed to create a logical switch %v, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	// If supported, enable IGMP snooping and querier on the node.
	if oc.multicastSupport {
		stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch",
			nodeName, "other-config:mcast_snoop=\"true\"")
		if err != nil {
			klog.Errorf("Failed to enable IGMP on logical switch %v, stdout: %q, stderr: %q, error: %v",
				nodeName, stdout, stderr, err)
			return err
		}

		// Configure querier only if we have an IPv4 address, otherwise
		// disable querier.
		if v4Gateway != nil {
			stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch",
				nodeName, "other-config:mcast_querier=\"true\"",
				"other-config:mcast_eth_src=\""+nodeLRPMAC.String()+"\"",
				"other-config:mcast_ip4_src=\""+v4Gateway.String()+"\"")
			if err != nil {
				klog.Errorf("Failed to enable IGMP Querier on logical switch %v, stdout: %q, stderr: %q, error: %v",
					nodeName, stdout, stderr, err)
				return err
			}
		} else {
			stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch",
				nodeName, "other-config:mcast_querier=\"false\"")
			if err != nil {
				klog.Errorf("Failed to disable IGMP Querier on logical switch %v, stdout: %q, stderr: %q, error: %v",
					nodeName, stdout, stderr, err)
				return err
			}
			klog.Infof("Disabled IGMP Querier on logical switch %v (No IPv4 Source IP available)",
				nodeName)
		}
	}

	// Connect the switch to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", nodeName, switchToRouterPrefix+nodeName,
		"--", "set", "logical_switch_port", switchToRouterPrefix+nodeName, "type=router",
		"options:router-port="+routerToSwitchPrefix+nodeName, "addresses="+"\""+nodeLRPMAC.String()+"\"")
	if err != nil {
		klog.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Add our cluster TCP and UDP load balancers to the node switch
	if oc.TCPLoadBalancerUUID == "" {
		return fmt.Errorf("TCP cluster load balancer not created")
	}
	stdout, stderr, err = util.RunOVNNbctl("set", "logical_switch", nodeName, "load_balancer="+oc.TCPLoadBalancerUUID)
	if err != nil {
		klog.Errorf("Failed to set logical switch %v's load balancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	// Add any service reject ACLs applicable for TCP LB
	acls := oc.getAllACLsForServiceLB(oc.TCPLoadBalancerUUID)
	if len(acls) > 0 {
		_, _, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "acls", strings.Join(acls, ","))
		if err != nil {
			klog.Warningf("Unable to add TCP reject ACLs: %s for switch: %s, error: %v", acls, nodeName, err)
		}
	}

	if oc.UDPLoadBalancerUUID == "" {
		return fmt.Errorf("UDP cluster load balancer not created")
	}
	stdout, stderr, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "load_balancer", oc.UDPLoadBalancerUUID)
	if err != nil {
		klog.Errorf("Failed to add logical switch %v's load balancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
		return err
	}

	// Add any service reject ACLs applicable for UDP LB
	acls = oc.getAllACLsForServiceLB(oc.UDPLoadBalancerUUID)
	if len(acls) > 0 {
		_, _, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "acls", strings.Join(acls, ","))
		if err != nil {
			klog.Warningf("Unable to add UDP reject ACLs: %s for switch: %s, error %v", acls, nodeName, err)
		}
	}

	if oc.SCTPSupport {
		if oc.SCTPLoadBalancerUUID == "" {
			return fmt.Errorf("SCTP cluster load balancer not created")
		}
		stdout, stderr, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "load_balancer", oc.SCTPLoadBalancerUUID)
		if err != nil {
			klog.Errorf("Failed to add logical switch %v's load balancer, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
			return err
		}

		// Add any service reject ACLs applicable for SCTP LB
		acls = oc.getAllACLsForServiceLB(oc.SCTPLoadBalancerUUID)
		if len(acls) > 0 {
			_, _, err = util.RunOVNNbctl("add", "logical_switch", nodeName, "acls", strings.Join(acls, ","))
			if err != nil {
				klog.Warningf("Unable to add SCTP reject ACLs: %s for switch: %s, error %v", acls, nodeName, err)
			}
		}
	}
	// Add the node to the logical switch cache
	return oc.lsManager.AddNode(nodeName, hostSubnets)
}

func (oc *Controller) addNodeAnnotations(node *kapi.Node, hostSubnets []*net.IPNet) error {
	nodeAnnotations, err := util.CreateNodeHostSubnetAnnotation(hostSubnets)
	if err != nil {
		return fmt.Errorf("failed to marshal node %q annotation for subnet %s",
			node.Name, util.JoinIPNets(hostSubnets, ","))
	}
	err = oc.kube.SetAnnotationsOnNode(node, nodeAnnotations)
	if err != nil {
		return fmt.Errorf("failed to set node-subnets annotation on node %s: %v",
			node.Name, err)
	}
	return nil
}

func (oc *Controller) addNode(node *kapi.Node) ([]*net.IPNet, error) {
	oc.clearInitialNodeNetworkUnavailableCondition(node, nil)

	hostSubnets, _ := util.ParseNodeHostSubnetAnnotation(node)
	if hostSubnets != nil {
		// Node already has subnet assigned; ensure its logical network is set up
		return hostSubnets, oc.ensureNodeLogicalNetwork(node.Name, hostSubnets)
	}

	// Node doesn't have a subnet assigned; reserve a new one for it
	hostSubnets, err := oc.masterSubnetAllocator.AllocateNetworks()
	if err != nil {
		return nil, fmt.Errorf("error allocating network for node %s: %v", node.Name, err)
	}
	klog.Infof("Allocated node %s HostSubnet %s", node.Name, util.JoinIPNets(hostSubnets, ","))

	defer func() {
		// Release the allocation on error
		if err != nil {
			for _, hostSubnet := range hostSubnets {
				_ = oc.masterSubnetAllocator.ReleaseNetwork(hostSubnet)
			}
		}
	}()

	// Ensure that the node's logical network has been created
	err = oc.ensureNodeLogicalNetwork(node.Name, hostSubnets)
	if err != nil {
		return nil, err
	}

	// Set the HostSubnet annotation on the node object to signal
	// to nodes that their logical infrastructure is set up and they can
	// proceed with their initialization
	err = oc.addNodeAnnotations(node, hostSubnets)
	if err != nil {
		return nil, err
	}

	return hostSubnets, nil
}

func (oc *Controller) deleteNodeHostSubnet(nodeName string, subnet *net.IPNet) error {
	err := oc.masterSubnetAllocator.ReleaseNetwork(subnet)
	if err != nil {
		return fmt.Errorf("error deleting subnet %v for node %q: %s", subnet, nodeName, err)
	}
	klog.Infof("Deleted HostSubnet %v for node %s", subnet, nodeName)
	return nil
}

func (oc *Controller) deleteNodeLogicalNetwork(nodeName string) error {
	// Remove the logical switch associated with the node
	if _, stderr, err := util.RunOVNNbctl("--if-exist", "ls-del", nodeName); err != nil {
		return fmt.Errorf("failed to delete logical switch %s, "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}

	// Remove the patch port that connects distributed router to node's logical switch
	if _, stderr, err := util.RunOVNNbctl("--if-exist", "lrp-del", routerToSwitchPrefix+nodeName); err != nil {
		return fmt.Errorf("failed to delete logical router port rtos-%s, "+
			"stderr: %q, error: %v", nodeName, stderr, err)
	}

	return nil
}

func (oc *Controller) deleteNode(nodeName string, hostSubnets []*net.IPNet,
	nodeLocalNatIPs []net.IP) error {
	// Clean up as much as we can but don't hard error
	for _, hostSubnet := range hostSubnets {
		if err := oc.deleteNodeHostSubnet(nodeName, hostSubnet); err != nil {
			klog.Errorf("Error deleting node %s HostSubnet %v: %v", nodeName, hostSubnet, err)
		}
	}
	if len(nodeLocalNatIPs) > 0 {
		if err := oc.nodeLocalNatIPAllocator.Release(nodeLocalNatIPs[0]); err != nil {
			klog.Errorf("Error deleting node %s's node local NAT IP %v: %v", nodeName, nodeLocalNatIPs, err)
		}
	}

	if err := oc.deleteNodeLogicalNetwork(nodeName); err != nil {
		klog.Errorf("Error deleting node %s logical network: %v", nodeName, err)
	}

	if err := gatewayCleanup(nodeName); err != nil {
		return fmt.Errorf("failed to clean up node %s gateway: (%v)", nodeName, err)
	}

	if err := oc.joinSwIPManager.releaseJoinLRPIPs(nodeName); err != nil {
		return err
	}

	if err := oc.deleteNodeChassis(nodeName); err != nil {
		return err
	}

	return nil
}

// OVN uses an overlay and doesn't need GCE Routes, we need to
// clear the NetworkUnavailable condition that kubelet adds to initial node
// status when using GCE (done here: https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/cloud/node_controller.go#L237).
// See discussion surrounding this here: https://github.com/kubernetes/kubernetes/pull/34398.
// TODO: make upstream kubelet more flexible with overlays and GCE so this
// condition doesn't get added for network plugins that don't want it, and then
// we can remove this function.
func (oc *Controller) clearInitialNodeNetworkUnavailableCondition(origNode, newNode *kapi.Node) {
	// If it is not a Cloud Provider node, then nothing to do.
	if origNode.Spec.ProviderID == "" {
		return
	}
	// if newNode is not nil, then we are called from UpdateFunc()
	if newNode != nil && reflect.DeepEqual(origNode.Status.Conditions, newNode.Status.Conditions) {
		return
	}

	cleared := false
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error

		oldNode, err := oc.kube.GetNode(origNode.Name)
		if err != nil {
			return err
		}
		// Informer cache should not be mutated, so get a copy of the object
		node := oldNode.DeepCopy()

		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == kapi.NodeNetworkUnavailable {
				condition := &node.Status.Conditions[i]
				if condition.Status != kapi.ConditionFalse && condition.Reason == "NoRouteCreated" {
					condition.Status = kapi.ConditionFalse
					condition.Reason = "RouteCreated"
					condition.Message = "ovn-kube cleared kubelet-set NoRouteCreated"
					condition.LastTransitionTime = metav1.Now()
					if err = oc.kube.UpdateNodeStatus(node); err == nil {
						cleared = true
					}
				}
				break
			}
		}
		return err
	})
	if resultErr != nil {
		klog.Errorf("Status update failed for local node %s: %v", origNode.Name, resultErr)
	} else if cleared {
		klog.Infof("Cleared node NetworkUnavailable/NoRouteCreated condition for %s", origNode.Name)
	}
}

// this is the worker function that does the periodic sync of nodes from kube API
// and sbdb and deletes chassis that are stale
func (oc *Controller) syncNodesPeriodic() {
	//node names is a slice of all node names
	nodes, err := oc.kube.GetNodes()
	if err != nil {
		klog.Errorf("Error getting existing nodes from kube API: %v", err)
		return
	}

	nodeNames := make([]string, 0, len(nodes.Items))

	for _, node := range nodes.Items {
		nodeNames = append(nodeNames, node.Name)
	}

	chassisData, stderr, err := util.RunOVNSbctl("--data=bare", "--no-heading",
		"--columns=name,hostname", "--format=json", "list", "Chassis")
	if err != nil {
		klog.Errorf("Failed to get chassis list: stderr: %s, error: %v",
			stderr, err)
		return
	}

	chassisMap, err := oc.unmarshalChassisDataIntoMap([]byte(chassisData))
	if err != nil {
		klog.Errorf("Failed to unmarshal chassis data into chassis map, error: %v: %s", err, chassisData)
		return
	}

	//delete existing nodes from the chassis map.
	for _, nodeName := range nodeNames {
		delete(chassisMap, nodeName)
	}

	for nodeName, chassisName := range chassisMap {
		if chassisName != "" {
			_, stderr, err = util.RunOVNSbctl("--if-exist", "chassis-del", chassisName)
			if err != nil {
				klog.Errorf("Failed to delete chassis with name %s for node %s: stderr: %s, error: %v",
					chassisName, nodeName, stderr, err)
			}
		}
	}
}

func (oc *Controller) getJoinLRPAddresses(nodeName string) []*net.IPNet {
	// try to get the IPs from the logical router port
	gwLRPIPs := []*net.IPNet{}
	gwLrpName := gwRouterToJoinSwitchPrefix + gwRouterPrefix + nodeName
	joinSubnets := oc.joinSwIPManager.lsm.GetSwitchSubnets(nodeName)
	networks, err := util.GetLrpNetworks(gwLrpName)
	if err == nil {
		for _, network := range networks {
			for _, subnet := range joinSubnets {
				if subnet.Contains(network.IP) {
					gwLRPIPs = append(gwLRPIPs, &net.IPNet{IP: network.IP, Mask: subnet.Mask})
					break
				}
			}
		}
	}

	if len(gwLRPIPs) != len(joinSubnets) {
		var errStr string
		if len(gwLRPIPs) == 0 {
			errStr = fmt.Sprintf("Failed to get IPs for logical router port %s", gwLrpName)
		} else {
			errStr = fmt.Sprintf("Invalid IPs %s (possibly not in the range of subnet %s)",
				util.JoinIPNetIPs(gwLRPIPs, " "), util.JoinIPNetIPs(joinSubnets, " "))
		}
		klog.Warningf("%s for logical router port %s", errStr, gwLrpName)
		return []*net.IPNet{}
	}
	return gwLRPIPs
}

func (oc *Controller) syncNodes(nodes []interface{}) {
	foundNodes := make(map[string]*kapi.Node)
	for _, tmp := range nodes {
		node, ok := tmp.(*kapi.Node)
		if !ok {
			klog.Errorf("Spurious object in syncNodes: %v", tmp)
			continue
		}
		foundNodes[node.Name] = node
		// For each existing node, reserve its joinSwitch LRP IPs if they already exist.
		gwLRPIPs := oc.getJoinLRPAddresses(node.Name)
		_ = oc.joinSwIPManager.reserveJoinLRPIPs(node.Name, gwLRPIPs)
	}

	// We only deal with cleaning up nodes that shouldn't exist here, since
	// watchNodes() will be called for all existing nodes at startup anyway.
	// Note that this list will include the 'join' cluster switch, which we
	// do not want to delete.

	chassisData, stderr, err := util.RunOVNSbctl("--data=bare", "--no-heading",
		"--columns=name,hostname", "--format=json", "list", "Chassis")
	if err != nil {
		klog.Errorf("Failed to get chassis list: stderr: %q, error: %v",
			stderr, err)
		return
	}

	chassisMap, err := oc.unmarshalChassisDataIntoMap([]byte(chassisData))
	if err != nil {
		klog.Errorf("Failed to unmarshal chassis data into chassis map, error: %v: %s", err, chassisData)
		return
	}

	//delete existing nodes from the chassis map.
	for nodeName := range foundNodes {
		delete(chassisMap, nodeName)
	}

	nodeSwitches, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name,other-config", "find", "logical_switch")
	if err != nil {
		klog.Errorf("Failed to get node logical switches: stderr: %q, error: %v",
			stderr, err)
		return
	}

	type NodeSubnets struct {
		hostSubnets []*net.IPNet
	}
	NodeSubnetsMap := make(map[string]*NodeSubnets)
	for _, result := range strings.Split(nodeSwitches, "\n\n") {
		// Split result into name and other-config
		items := strings.Split(result, "\n")
		if len(items) != 2 || len(items[0]) == 0 {
			continue
		}
		nodeName := items[0]
		if _, ok := foundNodes[nodeName]; ok {
			// node still exists, no cleanup to do
			continue
		}

		var subnets []*net.IPNet
		attrs := strings.Fields(items[1])
		for _, attr := range attrs {
			var subnet *net.IPNet
			if strings.HasPrefix(attr, "subnet=") {
				subnetStr := strings.TrimPrefix(attr, "subnet=")
				_, subnet, _ = net.ParseCIDR(subnetStr)
			} else if strings.HasPrefix(attr, "ipv6_prefix=") {
				prefixStr := strings.TrimPrefix(attr, "ipv6_prefix=")
				_, subnet, _ = net.ParseCIDR(prefixStr + "/64")
			}
			if subnet != nil {
				subnets = append(subnets, subnet)
			}
		}
		if len(subnets) == 0 {
			continue
		}

		var tmp NodeSubnets
		nodeSubnets, ok := NodeSubnetsMap[nodeName]
		if !ok {
			nodeSubnets = &tmp
			NodeSubnetsMap[nodeName] = nodeSubnets
		}
		nodeSubnets.hostSubnets = subnets
	}

	for nodeName, nodeSubnets := range NodeSubnetsMap {
		if err := oc.deleteNode(nodeName, nodeSubnets.hostSubnets, nil); err != nil {
			klog.Error(err)
		}
		//remove the node from the chassis map so we don't delete it twice
		delete(chassisMap, nodeName)
	}

	for nodeName, chassisName := range chassisMap {
		if chassisName != "" {
			_, stderr, err = util.RunOVNSbctl("--if-exist", "chassis-del", chassisName)
			if err != nil {
				klog.Errorf("Failed to delete chassis with name %s for logical switch %s: stderr: %q, error: %v",
					chassisName, nodeName, stderr, err)
			}
		}
	}
}

func (oc *Controller) unmarshalChassisDataIntoMap(chData []byte) (map[string]string, error) {
	//map of node name to chassis name
	chassisMap := make(map[string]string)

	type chassisList struct {
		Data     [][]string
		Headings []string
	}
	var mapUnmarshal chassisList

	if len(chData) == 0 {
		return chassisMap, nil
	}

	err := json.Unmarshal(chData, &mapUnmarshal)

	if err != nil {
		return nil, fmt.Errorf("error unmarshaling the chassis data: %s", err)
	}

	for _, chassis := range mapUnmarshal.Data {
		if len(chassis) < 2 || chassis[0] == "" || chassis[1] == "" {
			continue
		}
		chassisMap[chassis[1]] = chassis[0]
	}

	return chassisMap, nil
}

func (oc *Controller) deleteNodeChassis(nodeName string) error {
	chassisName, stderr, err := util.RunOVNSbctl("--data=bare", "--no-heading",
		"--columns=name", "find", "Chassis",
		"hostname="+nodeName)
	if err != nil {
		return fmt.Errorf("failed to get chassis name for node %s: stderr: %q, error: %v",
			nodeName, stderr, err)
	}

	if chassisName == "" {
		klog.Warningf("Chassis name is empty for node: %s", nodeName)
	} else {
		_, stderr, err = util.RunOVNSbctl("--if-exist", "chassis-del", chassisName)
		if err != nil {
			return fmt.Errorf("failed to delete chassis with name %s for node %s: stderr: %q, error: %v",
				chassisName, nodeName, stderr, err)
		}
	}

	return nil
}
