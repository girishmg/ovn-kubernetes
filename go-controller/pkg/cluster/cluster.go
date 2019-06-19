package cluster

import (
	"fmt"
	"net"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/client-go/kubernetes"
)

// OvnNodeController is the object holder for utilities meant for cluster management
type OvnNodeController struct {
	kube         kube.Interface
	watchFactory *factory.WatchFactory
}

// NewOvnNodeController creates a new controller for IP subnet allocation to
// a given resource type (either Namespace or Node)
func NewOvnNodeController(kubeClient kubernetes.Interface, wf *factory.WatchFactory) *OvnNodeController {
	return &OvnNodeController{
		kube:         &kube.Kube{KClient: kubeClient},
		watchFactory: wf,
	}
}

func setupOVNNode(nodeName string) error {
	// Tell ovn-*bctl how to talk to the database
	for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
		if err := auth.SetDBAuth(); err != nil {
			return err
		}
	}

	var err error

	nodeIP := config.Default.EncapIP
	if nodeIP == "" {
		nodeIP, err = netutils.GetNodeIP(nodeName)
		if err != nil {
			return fmt.Errorf("failed to obtain local IP from hostname %q: %v", nodeName, err)
		}
	} else {
		if ip := net.ParseIP(nodeIP); ip == nil {
			return fmt.Errorf("invalid encapsulation IP provided %q", nodeIP)
		}
	}

	_, stderr, err := util.RunOVSVsctl("set",
		"Open_vSwitch",
		".",
		fmt.Sprintf("external_ids:ovn-encap-type=%s", config.Default.EncapType),
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", nodeIP),
		fmt.Sprintf("external_ids:ovn-remote-probe-interval=%d",
			config.Default.InactivityProbe),
		fmt.Sprintf("external_ids:hostname=\"%s\"", nodeName),
	)
	if err != nil {
		return fmt.Errorf("error setting OVS external IDs: %v\n  %q", err, stderr)
	}
	return nil
}
