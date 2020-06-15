// +build linux

package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

// Creates br-local OVS bridge on the node and adds ovn-k8s-gw0 port to it with the
// appropriate IP and MAC address on it. All the traffic from this node's hostNetwork
// Pod towards cluster service ip whose backend is the node itself is forwarded to the
// ovn-k8s-gw0 port after SNATing by the OVN's distributed gateway port.
func setupLocalNodeAccessBridge(nodeName string, subnet *net.IPNet) error {
	localBridgeName := "br-local"
	_, stderr, err := util.RunOVSVsctl("--may-exist", "add-br", localBridgeName)
	if err != nil {
		return fmt.Errorf("failed to create bridge %s, stderr:%s (%v)",
			localBridgeName, stderr, err)
	}

	_, _, err = bridgedGatewayNodeSetup(nodeName, localBridgeName, localBridgeName,
		util.LocalNetworkName, true)
	if err != nil {
		return fmt.Errorf("failed while setting up local node access bridge : %v", err)
	}

	_, err = util.LinkSetUp(localBridgeName)
	if err != nil {
		return err
	}

	macAddress := util.IPAddrToHWAddr(net.ParseIP(util.V4NodeLocalNatSubnetNextHop)).String()
	_, stderr, err = util.RunOVSVsctl(
		"--may-exist", "add-port", localBridgeName, localnetGatewayNextHopPort,
		"--", "set", "interface", localnetGatewayNextHopPort, "type=internal",
		"mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		fmt.Sprintf("mac=%s", strings.ReplaceAll(macAddress, ":", "\\:")))
	if err != nil {
		return fmt.Errorf("failed to add the port %s to bridge %s, stderr:%s (%v)",
			localnetGatewayNextHopPort, localBridgeName, stderr, err)
	}

	link, err := util.LinkSetUp(localnetGatewayNextHopPort)
	if err != nil {
		return err
	}

	var gatewayNextHop net.IP
	var gatewaySubnetMask net.IPMask
	isSubnetIPv6 := utilnet.IsIPv6CIDR(subnet)
	if isSubnetIPv6 {
		gatewayNextHop = net.ParseIP(util.V6NodeLocalNatSubnetNextHop)
		gatewaySubnetMask = net.CIDRMask(util.V6NodeLocalNatSubnetPrefix, 128)
	} else {
		gatewayNextHop = net.ParseIP(util.V4NodeLocalNatSubnetNextHop)
		gatewaySubnetMask = net.CIDRMask(util.V4NodeLocalNatSubnetPrefix, 32)
	}
	gatewayNextHopCIDR := &net.IPNet{IP: gatewayNextHop, Mask: gatewaySubnetMask}

	// Flush all IP addresses on the nexthop port and add the new IP address
	if err = util.LinkAddrFlush(link); err == nil {
		err = util.LinkAddrAdd(link, gatewayNextHopCIDR)
	}
	return err
}

func addSharedGatewayIptRules(service *kapi.Service, nodeIP *net.IPNet) {
	rules := getGatewayIptRules(service, service.Spec.ClusterIP)
	if err := addIptRules(rules); err != nil {
		klog.Errorf("Failed to add iptables rules for service %s/%s: %v", service.Namespace, service.Name, err)
	}
}

func delSharedGatewayIptRules(service *kapi.Service, nodeIP *net.IPNet) {
	rules := getGatewayIptRules(service, service.Spec.ClusterIP)
	if err := delIptRules(rules); err != nil {
		klog.Errorf("Failed to delete iptables rules for service %s/%s: %v", service.Namespace, service.Name, err)
	}
}

func syncSharedGatewayIptRules(services []interface{}) {
	keepIPTRules := []iptRule{}
	for _, service := range services {
		svc, ok := service.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncSharedGatewayIptRules: %v", service)
			continue
		}
		keepIPTRules = append(keepIPTRules, getGatewayIptRules(svc, svc.Spec.ClusterIP)...)
	}
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		removeStaleIPTRules("nat", chain, keepIPTRules)
		removeStaleIPTRules("filter", chain, keepIPTRules)
	}
}
