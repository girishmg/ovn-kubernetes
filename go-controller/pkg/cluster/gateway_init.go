package cluster

import (
	"fmt"
	"net"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// bridgedGatewayNodeSetup makes the bridge's MAC address permanent, sets up
// the physical network name mappings for the bridge, and returns an ifaceID
// created from the bridge name and the node name
func bridgedGatewayNodeSetup(nodeName, bridgeInterface string, physicalNetworkName string) (string, string, error) {
	// A OVS bridge's mac address can change when ports are added to it.
	// We cannot let that happen, so make the bridge mac address permanent.
	macAddress, err := util.GetOVSPortMACAddress(bridgeInterface)
	if err != nil {
		return "", "", err
	}
	stdout, stderr, err := util.RunOVSVsctl("set", "bridge",
		bridgeInterface, "other-config:hwaddr="+macAddress)
	if err != nil {
		return "", "", fmt.Errorf("Failed to set bridge, stdout: %q, stderr: %q, "+
			"error: %v", stdout, stderr, err)
	}

	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network. It is in the form of physnet1:br1,physnet2:br2.
	// Note that there may be multiple ovs bridge mappings, be sure not to override
	// the mappings for the other physical network
	stdout, stderr, err = util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return "", "", fmt.Errorf("Failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	// skip the existing mapping setting for the specified physicalNetworkName
	mapString := ""
	bridge_mappings := strings.Split(stdout, ",")
	for _, bridge_mapping := range bridge_mappings {
		m := strings.Split(bridge_mapping, ":")
		if network := m[0]; network != physicalNetworkName {
			if len(mapString) != 0 {
				mapString += ","
			}
			mapString += bridge_mapping
		}
	}
	if len(mapString) != 0 {
		mapString += ","
	}
	mapString += physicalNetworkName+":"+bridgeInterface

	_, stderr, err = util.RunOVSVsctl("set", "Open_vSwitch", ".",
		fmt.Sprintf("external_ids:ovn-bridge-mappings=%s", mapString))
	if err != nil {
		return "", "", fmt.Errorf("Failed to set ovn-bridge-mappings for ovs bridge %s"+
			", stderr:%s (%v)", bridgeInterface, stderr, err)
	}

	ifaceID := bridgeInterface + "_" + nodeName
	return ifaceID, macAddress, nil
}

// getIPv4Address returns the ipv4 address for the network interface 'iface'.
func getIPv4Address(iface string) (string, error) {
	var ipAddress string
	intf, err := net.InterfaceByName(iface)
	if err != nil {
		return ipAddress, err
	}

	addrs, err := intf.Addrs()
	if err != nil {
		return ipAddress, err
	}
loop:
	for _, addr := range addrs {
		switch ip := addr.(type) {
		case *net.IPNet:
			if ip.IP.To4() != nil {
				ipAddress = ip.String()
			}
			// get the first ip address
			if ipAddress != "" {
				break loop
			}
		}
	}
	return ipAddress, nil
}

func (cluster *OvnClusterController) initGateway(
	nodeName string, clusterIPSubnet []string, subnet string) error {

	if config.Gateway.NodeportEnable {
		err := initLoadBalancerHealthChecker(nodeName, cluster.watchFactory)
		if err != nil {
			return err
		}
	}

	if config.Gateway.Mode == config.GatewayModeLocal {
		return initLocalnetGateway(nodeName, clusterIPSubnet, subnet,
			cluster.watchFactory)
	}

	gatewayNextHop := config.Gateway.NextHop
	gatewayIntf := config.Gateway.Interface
	if gatewayNextHop == "" || gatewayIntf == "" {
		// We need to get the interface details from the default gateway.
		defaultGatewayIntf, defaultGatewayNextHop, err := getDefaultGatewayInterfaceDetails()
		if err != nil {
			return err
		}

		if gatewayNextHop == "" {
			gatewayNextHop = defaultGatewayNextHop
		}

		if gatewayIntf == "" {
			gatewayIntf = defaultGatewayIntf
		}
	}

	var err error
	if config.Gateway.Mode == config.GatewayModeSpare {
		err = initSpareGateway(nodeName, clusterIPSubnet, subnet,
			gatewayNextHop, gatewayIntf)
	} else if config.Gateway.Mode == config.GatewayModeShared {
		err = initSharedGateway(nodeName, clusterIPSubnet, subnet,
			gatewayNextHop, gatewayIntf, cluster.watchFactory)
	}

	return err
}

// CleanupClusterNode cleans up OVS resources on the k8s node on ovnkube-node daemonset deletion.
// This is going to be a best effort cleanup.
func CleanupClusterNode(name string, kube *kube.Kube) error {
	node, err := kube.GetNode(name)
	if err != nil {
		logrus.Errorf("Failed to get kubernetes node %q, error: %v", name, err)
		return nil
	}

	switch config.Gateway.Mode {
	case config.GatewayModeLocal:
		err = cleanupLocalnetGateway(util.PhysicalNetworkName)
	case config.GatewayModeSpare:
		nodeName := strings.ToLower(node.Name)
		err = cleanupSpareGateway(config.Gateway.Interface, nodeName)
	case config.GatewayModeShared:
		err1 := cleanupLocalnetGateway(util.LocalNetworkName)
		err2 := cleanupSharedGateway()
		if err1 == nil {
			err = err2
		} else if err2 == nil {
			err = err1
		} else {
			err = fmt.Errorf("Combined errors: %v, %v", err1, err2)
		}
	}
	if err != nil {
		logrus.Errorf("Failed to cleanup Gateway, error: %v", err)
	}

	// Make sure br-int is deleted, the management internal port is also deleted at the same time.
	stdout, stderr, err := util.RunOVSVsctl("--", "--if-exists", "del-br", "br-int")
	if err != nil {
		logrus.Errorf("Failed to delete bridge br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
	}

	stdout, stderr, err = util.RunOVSVsctl("--", "--if-exists", "remove", "Open_vSwitch", ".", "external_ids", "ovn-bridge-mappings")
	if err != nil {
			logrus.Errorf("Failed to delete ovn-bridge-mappings, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
	}

	return nil
}
