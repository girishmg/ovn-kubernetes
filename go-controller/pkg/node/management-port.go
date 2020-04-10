package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog"
)

// K8s mgmt interface name to be used as an OVS internal port on the node
const (
	k8sMgmtIntfName = "ovn-k8s-mp0"
)

func (n *OvnNode) createManagementPort(localSubnet *net.IPNet, nodeAnnotator kube.Annotator,
	waiter *startupWaiter) error {
	// Retrieve the routerIP and mangementPortIP for a given localSubnet
	routerIP, portIP := util.GetNodeWellKnownAddresses(localSubnet)
	routerMAC := util.IPAddrToHWAddr(routerIP.IP)

	// Kubernetes emits events when pods are created. The event will contain
	// only lowercase letters of the hostname even though the kubelet is
	// started with a hostname that contains lowercase and uppercase letters.
	// When the kubelet is started with a hostname containing lowercase and
	// uppercase letters, this causes a mismatch between what the watcher
	// will try to fetch and what kubernetes provides, thus failing to
	// create the port on the logical switch.
	// Until the above is changed, switch to a lowercase hostname
	nodeName := strings.ToLower(n.name)

	// Make sure br-int is created.
	stdout, stderr, err := util.RunOVSVsctl("--", "--may-exist", "add-br", "br-int")
	if err != nil {
		klog.Errorf("Failed to create br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Create a OVS internal interface.
	legacyMgmtIntfName := util.GetLegacyK8sMgmtIntfName(nodeName)
	stdout, stderr, err = util.RunOVSVsctl(
		"--", "--if-exists", "del-port", "br-int", legacyMgmtIntfName,
		"--", "--may-exist", "add-port", "br-int", k8sMgmtIntfName,
		"--", "set", "interface", k8sMgmtIntfName,
		"type=internal", "mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		"external-ids:iface-id=k8s-"+nodeName)
	if err != nil {
		klog.Errorf("Failed to add port to br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}
	macAddress, err := util.GetOVSPortMACAddress(k8sMgmtIntfName)
	if err != nil {
		klog.Errorf("Failed to get management port MAC address: %v", err)
		return err
	}
	// persist the MAC address so that upon node reboot we get back the same mac address.
	_, stderr, err = util.RunOVSVsctl("set", "interface", k8sMgmtIntfName,
		fmt.Sprintf("mac=%s", strings.ReplaceAll(macAddress.String(), ":", "\\:")))
	if err != nil {
		klog.Errorf("failed to persist MAC address %q for %q: stderr:%s (%v)", macAddress.String(),
			k8sMgmtIntfName, stderr, err)
		return err
	}

	err = createPlatformManagementPort(k8sMgmtIntfName, portIP, routerIP.IP, routerMAC, n.stopChan)
	if err != nil {
		return err
	}

	if err := util.SetNodeManagementPortMACAddress(nodeAnnotator, macAddress); err != nil {
		return err
	}

	waiter.AddWait(managementPortReady, nil)
	return nil
}

// managementPortReady will check to see if OpenFlow rules for management port has been created
func managementPortReady() (bool, error) {
	// Get the OVS interface name for the Management Port
	ofport, _, err := util.RunOVSVsctl("--if-exists", "get", "interface", k8sMgmtIntfName, "ofport")
	if err != nil {
		return false, nil
	}

	// OpenFlow table 65 performs logical-to-physical translation. It matches the packet’s logical
	// egress  port. Its actions output the packet to the port attached to the OVN integration bridge
	// that represents that logical  port.
	stdout, _, err := util.RunOVSOfctl("--no-stats", "--no-names", "dump-flows", "br-int",
		"table=65,out_port="+ofport)
	if err != nil {
		return false, nil
	}
	if !strings.Contains(stdout, "actions=output:"+ofport) {
		return false, nil
	}
	return true, nil
}
