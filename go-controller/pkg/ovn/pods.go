package ovn

import (
	"fmt"
	"net"
	"strings"
	"time"

	goovn "github.com/ebay/go-ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

// Builds the logical switch port name for a given pod.
func podLogicalPortName(pod *kapi.Pod) string {
	return pod.Namespace + "_" + pod.Name
}

func (oc *Controller) syncPods(pods []interface{}) {
	// get the list of logical switch ports (equivalent to pods)
	expectedLogicalPorts := make(map[string]bool)
	for _, podInterface := range pods {
		pod, ok := podInterface.(*kapi.Pod)
		if !ok {
			klog.Errorf("Spurious object in syncPods: %v", podInterface)
			continue
		}
		annotations, err := util.UnmarshalPodAnnotation(pod.Annotations)
		if podScheduled(pod) && podWantsNetwork(pod) && err == nil {
			logicalPort := podLogicalPortName(pod)
			expectedLogicalPorts[logicalPort] = true
			if err = oc.lsManager.AllocateIPs(pod.Spec.NodeName, annotations.IPs); err != nil {
				klog.Errorf("Couldn't allocate IPs: %s for pod: %s on node: %s"+
					" error: %v", util.JoinIPNetIPs(annotations.IPs, " "), logicalPort,
					pod.Spec.NodeName, err)
			}
		}
	}

	// get the list of logical ports from OVN
	output, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=name", "find", "logical_switch_port", "external_ids:pod=true")
	if err != nil {
		klog.Errorf("Error in obtaining list of logical ports, "+
			"stderr: %q, err: %v",
			stderr, err)
		return
	}
	existingLogicalPorts := strings.Fields(output)
	for _, existingPort := range existingLogicalPorts {
		if _, ok := expectedLogicalPorts[existingPort]; !ok {
			// not found, delete this logical port
			klog.Infof("Stale logical port found: %s. This logical port will be deleted.", existingPort)
			out, stderr, err := util.RunOVNNbctl("--if-exists", "lsp-del",
				existingPort)
			if err != nil {
				klog.Errorf("Error in deleting pod's logical port "+
					"stdout: %q, stderr: %q err: %v",
					out, stderr, err)
			}
		}
	}
}

func (oc *Controller) deleteLogicalPort(pod *kapi.Pod) {
	if pod.Spec.HostNetwork {
		return
	}

	podDesc := pod.Namespace + "/" + pod.Name
	klog.Infof("Deleting pod: %s", podDesc)

	logicalPort := podLogicalPortName(pod)

	// logical switch port may be created even the port is not added to the cache, delete it first
	out, stderr, err := util.RunOVNNbctl("--if-exists", "lsp-del", logicalPort)
	if err != nil {
		klog.Errorf("Error in deleting pod %s logical port "+
			"stdout: %q, stderr: %q, (%v)",
			podDesc, out, stderr, err)
	}

	portInfo, err := oc.logicalPortCache.get(logicalPort)
	if err != nil {
		klog.Errorf(err.Error())
		// if the pod is not in the cache, the IPs may already been reserved in syncPods()
		nodeName := pod.Spec.NodeName
		if nodeName != "" {
			annotations, err := util.UnmarshalPodAnnotation(pod.Annotations)
			if err == nil {
				podIPs := annotations.IPs
				if len(podIPs) != 0 {
					if err := oc.lsManager.ReleaseIPs(nodeName, podIPs); err != nil {
						klog.Errorf(err.Error())
					}
				}
			}
		}
		return
	}

	// FIXME: if any of these steps fails we need to stop and try again later...

	// Remove the port from the default deny multicast policy
	if oc.multicastSupport {
		if err := podDeleteDefaultDenyMulticastPolicy(portInfo); err != nil {
			klog.Errorf(err.Error())
		}
	}

	if err := oc.deletePodFromNamespace(pod.Namespace, portInfo); err != nil {
		klog.Errorf(err.Error())
	}

	if err := oc.lsManager.ReleaseIPs(portInfo.logicalSwitch, portInfo.ips); err != nil {
		klog.Errorf(err.Error())
	}

	oc.logicalPortCache.remove(logicalPort)
}

func (oc *Controller) waitForNodeLogicalSwitch(nodeName string) error {
	// Wait for the node logical switch to be created by the ClusterController.
	// The node switch will be created when the node's logical network infrastructure
	// is created by the node watch.
	if err := wait.PollImmediate(30*time.Millisecond, 30*time.Second, func() (bool, error) {
		return oc.lsManager.GetSwitchSubnets(nodeName) != nil, nil
	}); err != nil {
		return fmt.Errorf("timed out waiting for logical switch %q subnet: %v", nodeName, err)
	}
	return nil
}

func (oc *Controller) addRoutesGatewayIP(pod *kapi.Pod, podAnnotation *util.PodAnnotation, nodeSubnets []*net.IPNet) error {
	// if there are other network attachments for the pod, then check if those network-attachment's
	// annotation has default-route key. If present, then we need to skip adding default route for
	// OVN interface
	networks, err := util.GetPodNetSelAnnotation(pod, util.NetworkAttachmentAnnotation)
	if err != nil {
		return fmt.Errorf("error while getting network attachment definition for [%s/%s]: %v",
			pod.Namespace, pod.Name, err)
	}
	otherDefaultRoute := false
	for _, network := range networks {
		if len(network.GatewayRequest) != 0 && network.GatewayRequest[0] != nil {
			otherDefaultRoute = true
			break
		}
	}
	// DUALSTACK FIXME: hybridOverlayExternalGW is not Dualstack
	var hybridOverlayExternalGW net.IP
	if config.HybridOverlay.Enabled {
		hybridOverlayExternalGW, err = oc.getHybridOverlayExternalGwAnnotation(pod.Namespace)
		if err != nil {
			return err
		}
	}

	for _, podIfAddr := range podAnnotation.IPs {
		isIPv6 := utilnet.IsIPv6CIDR(podIfAddr)
		nodeSubnet, err := util.MatchIPFamily(isIPv6, nodeSubnets)
		if err != nil {
			return err
		}
		// DUALSTACK FIXME: hybridOverlayExternalGW is not Dualstack
		// When oc.getHybridOverlayExternalGwAnnotation() supports dualstack, return error if no match.
		// If external gateway mode is configured, need to use it for all outgoing traffic, so don't want
		// to fall back to the default gateway here
		if hybridOverlayExternalGW != nil && utilnet.IsIPv6(hybridOverlayExternalGW) != isIPv6 {
			klog.Warningf("Pod %s/%s has no external gateway for %s", pod.Namespace, pod.Name, util.IPFamilyName(isIPv6))
			continue
		}

		gatewayIPnet := util.GetNodeGatewayIfAddr(nodeSubnet)
		var gatewayIP net.IP
		if otherDefaultRoute || hybridOverlayExternalGW != nil {
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
					Dest:    clusterSubnet.CIDR,
					NextHop: gatewayIPnet.IP,
				})
			}
			for _, serviceSubnet := range config.Kubernetes.ServiceCIDRs {
				podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
					Dest:    serviceSubnet,
					NextHop: gatewayIPnet.IP,
				})
			}
			if hybridOverlayExternalGW != nil {
				gatewayIP = util.GetNodeHybridOverlayIfAddr(nodeSubnet).IP
			}
		} else {
			gatewayIP = gatewayIPnet.IP
		}

		if len(config.HybridOverlay.ClusterSubnets) > 0 {
			// Add a route for each hybrid overlay subnet via the hybrid
			// overlay port on the pod's logical switch.
			nextHop := util.GetNodeHybridOverlayIfAddr(nodeSubnet).IP
			for _, clusterSubnet := range config.HybridOverlay.ClusterSubnets {
				if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) == isIPv6 {
					podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
						Dest:    clusterSubnet.CIDR,
						NextHop: nextHop,
					})
				}
			}
		}
		if gatewayIP != nil {
			podAnnotation.Gateways = append(podAnnotation.Gateways, gatewayIP)
		}
	}
	return nil
}

func (oc *Controller) getHybridOverlayExternalGwAnnotation(ns string) (net.IP, error) {
	nsInfo, err := oc.waitForNamespaceLocked(ns)
	if err != nil {
		return nil, err
	}
	defer nsInfo.Unlock()
	return nsInfo.hybridOverlayExternalGW, nil
}

func (oc *Controller) addLogicalPort(pod *kapi.Pod) (err error) {
	// If a node does node have an assigned hostsubnet don't wait for the logical switch to appear
	if oc.lsManager.IsNonHostSubnetSwitch(pod.Spec.NodeName) {
		return nil
	}

	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort took %v", pod.Namespace, pod.Name, time.Since(start))
	}()

	logicalSwitch := pod.Spec.NodeName
	err = oc.waitForNodeLogicalSwitch(logicalSwitch)
	if err != nil {
		return err
	}

	portName := podLogicalPortName(pod)
	klog.V(5).Infof("Creating logical port for %s on switch %s", portName, logicalSwitch)

	var podMac net.HardwareAddr
	var podIfAddrs []*net.IPNet
	var cmds []*goovn.OvnCommand
	var addresses []string
	var cmd *goovn.OvnCommand
	var podAddedInCache bool

	annotation, _ := util.UnmarshalPodAnnotation(pod.Annotations)

	// Check if the pod's logical switch port already exists. If it
	// does don't re-add the port to OVN as this will change its
	// UUID and and the port cache, address sets, and port groups
	// will still have the old UUID.
	// if portName is already in the cache, the Pod logical switch port is already created with IP addresses allocated
	portInfo, err := oc.logicalPortCache.get(portName)
	if err == nil {
		podIfAddrs = portInfo.ips
		podMac = portInfo.mac
	} else {
		// If the pod does not exist in the cache, it is either:
		// 	- ovnkube-master restarts and the POD annotation is already created (Pod logical switch port
		// 	  may or may not exists, but IPs are already reserved)
		//  - Both Pod annotation and logical switch port do not exist, IPs are not allocated either.

		// Check if the pod's logical switch port already exists. If it
		// does don't re-add the port to OVN as this will change its
		// UUID and and the port cache, address sets, and port groups
		// will still have the old UUID.
		lsp, err := oc.ovnNBClient.LSPGet(portName)
		if err != nil && err != goovn.ErrorNotFound && err != goovn.ErrorSchema {
			return fmt.Errorf("unable to get the lsp: %s from the nbdb: %s", portName, err)
		}

		if lsp == nil {
			cmd, err = oc.ovnNBClient.LSPAdd(logicalSwitch, portName)
			if err != nil {
				return fmt.Errorf("unable to create the LSPAdd command for port: %s from the nbdb", portName)
			}
			cmds = append(cmds, cmd)
		}

		if annotation != nil {
			// if Pod annoation already exists, its IPs should already be reserved in syncPods()
			podMac = annotation.MAC
			podIfAddrs = annotation.IPs

			// If the pod already has annotations use the existing static
			// IP/MAC from the annotation.
			cmd, err = oc.ovnNBClient.LSPSetDynamicAddresses(portName, "")
			if err != nil {
				return fmt.Errorf("unable to create LSPSetDynamicAddresses command for port: %s", portName)
			}
			cmds = append(cmds, cmd)
		} else {
			podMac, podIfAddrs, err = oc.assignPodAddresses(logicalSwitch)
			if err != nil {
				return fmt.Errorf("failed to assign pod addresses for pod %s on node: %s, err: %v",
					portName, logicalSwitch, err)
			}

			// Once the pod is added to the port cache, do not release its IPs.
			defer func(nodeName string) {
				if !podAddedInCache {
					if relErr := oc.lsManager.ReleaseIPs(nodeName, podIfAddrs); relErr != nil {
						klog.Errorf("Error when releasing IPs for node: %s, err: %q",
							nodeName, relErr)
					} else {
						klog.V(5).Infof("Released IPs: %s for node: %s", util.JoinIPNetIPs(podIfAddrs, " "), nodeName)
					}
				}
			}(logicalSwitch)

			var networks []*types.NetworkSelectionElement

			networks, err = util.GetPodNetSelAnnotation(pod, util.DefNetworkAnnotation)
			// handle error cases separately first to ensure binding to err, otherwise the
			// defer will fail
			if err != nil {
				return fmt.Errorf("error while getting custom MAC config for port %q from "+
					"default-network's network-attachment: %v", portName, err)
			} else if networks != nil && len(networks) != 1 {
				err = fmt.Errorf("invalid network annotation size while getting custom MAC config"+
					" for port %q", portName)
				return err
			}

			if networks != nil && networks[0].MacRequest != "" {
				klog.V(5).Infof("Pod %s/%s requested custom MAC: %s", pod.Namespace, pod.Name, networks[0].MacRequest)
				podMac, err = net.ParseMAC(networks[0].MacRequest)
				if err != nil {
					return fmt.Errorf("failed to parse mac %s requested in annotation for pod %s: Error %v",
						networks[0].MacRequest, pod.Name, err)
				}
			}
		}

		// set addresses on the port
		addresses = make([]string, len(podIfAddrs)+1)
		addresses[0] = podMac.String()
		for idx, podIfAddr := range podIfAddrs {
			addresses[idx+1] = podIfAddr.IP.String()
		}
		cmd, err = oc.ovnNBClient.LSPSetAddress(portName, addresses...)
		if err != nil {
			return fmt.Errorf("unable to create LSPSetAddress command for port: %s", portName)
		}
		cmds = append(cmds, cmd)

		// add external ids
		extIds := map[string]string{"namespace": pod.Namespace, "pod": "true"}
		cmd, err = oc.ovnNBClient.LSPSetExternalIds(portName, extIds)
		if err != nil {
			return fmt.Errorf("unable to create LSPSetAddress command for port: %s", portName)
		}
		cmds = append(cmds, cmd)

		// add port security addresses
		cmd, err = oc.ovnNBClient.LSPSetPortSecurity(portName, addresses...)
		if err != nil {
			return fmt.Errorf("unable to create LSPSetPortSecurity command for port: %s", portName)
		}
		cmds = append(cmds, cmd)

		// execute all the commands together.
		err = oc.ovnNBClient.Execute(cmds...)
		if err != nil {
			return fmt.Errorf("error while creating logical port %s error: %v",
				portName, err)
		}

		// if pod failed to be added to the cache, and the Pod IPs are newly allocated, delete the Pod's
		// logical switch port (Its IPs will also be released. See defer function above)
		defer func() {
			if !podAddedInCache && annotation == nil {
				cmd, err = oc.ovnNBClient.LSPDel(portName)
				if err != nil {
					klog.Errorf("Unable to create the LSPDel command for port: %s from the nbdb", portName)
					return
				}
				err = oc.ovnNBClient.Execute(cmd)
				if err != nil {
					klog.Errorf("Error while deleting logical port %s error: %v", portName, err)
				}
			}
		}()

		lsp, err = oc.ovnNBClient.LSPGet(portName)
		if err != nil || lsp == nil {
			return fmt.Errorf("failed to get the logical switch port: %s from the ovn client, error: %s", portName, err)
		}

		// Add the pod's logical switch port to the port cache
		portInfo = oc.logicalPortCache.add(logicalSwitch, portName, lsp.UUID, podMac, podIfAddrs)
		podAddedInCache = true
	}

	// Enforce the default deny multicast policy
	if oc.multicastSupport {
		if err = podAddDefaultDenyMulticastPolicy(portInfo); err != nil {
			return err
		}
	}

	if err = oc.addPodToNamespace(pod.Namespace, portInfo); err != nil {
		return err
	}

	if annotation == nil {
		podAnnotation := util.PodAnnotation{
			IPs: podIfAddrs,
			MAC: podMac,
		}
		var nodeSubnets []*net.IPNet
		if nodeSubnets = oc.lsManager.GetSwitchSubnets(logicalSwitch); nodeSubnets == nil {
			return fmt.Errorf("cannot retrieve subnet for assigning gateway routes for pod %s, node: %s",
				pod.Name, logicalSwitch)
		}
		err = oc.addRoutesGatewayIP(pod, &podAnnotation, nodeSubnets)
		if err != nil {
			return err
		}
		var marshalledAnnotation map[string]string
		marshalledAnnotation, err = util.MarshalPodAnnotation(&podAnnotation)
		if err != nil {
			return fmt.Errorf("error creating pod network annotation: %v", err)
		}

		klog.V(5).Infof("Annotation values: ip=%v ; mac=%s ; gw=%s\nAnnotation=%s",
			podIfAddrs, podMac, podAnnotation.Gateways, marshalledAnnotation)
		if err = oc.kube.SetAnnotationsOnPod(pod, marshalledAnnotation); err != nil {
			return fmt.Errorf("failed to set annotation on pod %s: %v", pod.Name, err)
		}

		// observe the pod creation latency metric.
		metrics.RecordPodCreated(pod)
	}

	return nil
}

// Given a node, gets the next set of addresses (from the IPAM) for each of the node's
// subnets to assign to the new pod
func (oc *Controller) assignPodAddresses(nodeName string) (net.HardwareAddr, []*net.IPNet, error) {
	var (
		podMAC   net.HardwareAddr
		podCIDRs []*net.IPNet
		err      error
	)
	podCIDRs, err = oc.lsManager.AllocateNextIPs(nodeName)
	if err != nil {
		return nil, nil, err
	}
	if len(podCIDRs) > 0 {
		podMAC = util.IPAddrToHWAddr(podCIDRs[0].IP)
	}
	return podMAC, podCIDRs, nil
}
