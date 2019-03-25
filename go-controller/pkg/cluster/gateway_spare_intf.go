package cluster

import (
	"fmt"
	"strings"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
)

func initSpareGateway(nodeName string, clusterIPSubnet []string,
	subnet, gwNextHop, gwIntf string, gwVLANId uint, nodeportEnable bool) error {

	// Now, we get IP address from physical interface. If IP does not
	// exists error out.
	ipAddress, err := getIPv4Address(gwIntf)
	if err != nil {
		return fmt.Errorf("Failed to get interface details for %s (%v)",
			gwIntf, err)
	}
	if ipAddress == "" {
		return fmt.Errorf("%s does not have a ipv4 address", gwIntf)
	}
	err = util.GatewayInit(clusterIPSubnet, nodeName, ipAddress,
		gwIntf, "", gwNextHop, subnet, gwVLANId, nodeportEnable)
	if err != nil {
		return fmt.Errorf("failed to init spare interface gateway: %v", err)
	}

	return nil
}

func cleanupSpareGateway(nodeName string) (error) {
	stdout, stderr, err := util.RunOVSVsctl("list-ports", "br-int")
	if err != nil {
		return fmt.Errorf("Failed to get ports of br-int stderr:%s (%v)", stderr, err)
	}
	for _, physicalInterface := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		if strings.HasPrefix(physicalInterface, "k8s-") {
			continue
		}
		stdout, stderr, err := util.RunOVSVsctl("--", "--if-exists", "del-port", "br-int", physicalInterface)
		if err != nil {
			logrus.Errorf("Failed to delete port %s on br-int, stdout: %q, stderr: %q, error: %v",
				physicalInterface, stdout, stderr, err)
		}
		if nodeName != "" {
			stdout, stderr, err = util.RunOVNNbctl("--if-exists", "get",
				"logical_router_port", "rtoe-GR_"+nodeName, "networks")
			if err != nil {
				logrus.Errorf("Failed to get network on logical router port rtoe-GR_%s, stdout: %q, stderr: %q, error: %v",
					nodeName, stdout, stderr, err)
			} else {
				nicIP := strings.Trim(stdout, "[]\"")
				_, _, err = util.RunIP("addr", "add", nicIP, "dev", physicalInterface)
				if err != nil {
					logrus.Errorf("Failed to add IP address %s back to interface %s, error: %v",
						nicIP, physicalInterface, err)
				}
			}
		}
	}

	return nil
}