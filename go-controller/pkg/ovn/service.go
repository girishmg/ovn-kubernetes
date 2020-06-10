package ovn

import (
	"fmt"
	"net"
	"reflect"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/reference"
	"k8s.io/klog"
)

func (ovn *Controller) syncServices(services []interface{}) {
	// For all clusterIP in k8s, we will populate the below slice with
	// IP:port. In OVN's database those are the keys. We need to
	// have separate slice for TCP, SCTP, and UDP load-balancers (hence the dict).
	clusterServices := make(map[kapi.Protocol][]string)

	// For all nodePorts in k8s, we will populate the below slice with
	// nodePort. In OVN's database, nodeIP:nodePort is the key.
	// We have separate slice for TCP, SCTP, and UDP nodePort load-balancers.
	// We will get nodeIP separately later.
	nodeportServices := make(map[kapi.Protocol][]string)

	// For all externalIPs in k8s, we will populate the below map of slices
	// with loadbalancer type services based on each protocol.
	lbServices := make(map[kapi.Protocol][]string)

	// Go through the k8s services and populate 'clusterServices',
	// 'nodeportServices' and 'lbServices'
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v", serviceInterface)
			continue
		}

		if !util.ServiceTypeHasClusterIP(service) {
			continue
		}

		if !util.IsClusterIPSet(service) {
			klog.V(5).Infof("Skipping service %s due to clusterIP = %q", service.Name, service.Spec.ClusterIP)
			continue
		}

		for _, svcPort := range service.Spec.Ports {
			if err := util.ValidatePort(svcPort.Protocol, svcPort.Port); err != nil {
				klog.Errorf("Error validating port %s: %v", svcPort.Name, err)
				continue
			}

			if util.ServiceTypeHasNodePort(service) {
				port := fmt.Sprintf("%d", svcPort.NodePort)
				nodeportServices[svcPort.Protocol] = append(nodeportServices[svcPort.Protocol], port)
			}

			key := util.JoinHostPortInt32(service.Spec.ClusterIP, svcPort.Port)
			clusterServices[svcPort.Protocol] = append(clusterServices[svcPort.Protocol], key)

			for _, extIP := range service.Spec.ExternalIPs {
				key := util.JoinHostPortInt32(extIP, svcPort.Port)
				lbServices[svcPort.Protocol] = append(lbServices[svcPort.Protocol], key)
			}
		}
	}

	// Get OVN's current cluster load-balancer VIPs and delete them if they
	// are stale.
	for _, protocol := range []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolUDP, kapi.ProtocolSCTP} {
		loadBalancer, err := ovn.getLoadBalancer(protocol)
		if err != nil {
			klog.Errorf("Failed to get load-balancer for %s (%v)", kapi.Protocol(protocol), err)
			continue
		}

		loadBalancerVIPs, err := ovn.getLoadBalancerVIPs(loadBalancer)
		if err != nil {
			klog.Errorf("failed to get load-balancer vips for %s (%v)", loadBalancer, err)
			continue
		}
		for vip := range loadBalancerVIPs {
			if !stringSliceMembership(clusterServices[protocol], vip) {
				klog.V(5).Infof("Deleting stale cluster vip %s in loadbalancer %s", vip, loadBalancer)
				if err := ovn.deleteLoadBalancerVIP(loadBalancer, vip); err != nil {
					klog.Error(err)
				}
			}
		}
	}

	// For each gateway, remove any VIP that does not exist in
	// 'nodeportServices'.
	gateways, stderr, err := ovn.getOvnGateways()
	if err != nil {
		klog.Errorf("failed to get ovn gateways. Not syncing nodeport "+
			"stdout: %q, stderr: %q (%v)", gateways, stderr, err)
		return
	}

	for _, gateway := range gateways {
		for _, protocol := range []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolUDP, kapi.ProtocolSCTP} {
			loadBalancer, err := ovn.getGatewayLoadBalancer(gateway, protocol)
			if err != nil {
				klog.Errorf("physical gateway %s does not have load_balancer (%v)", gateway, err)
				continue
			}
			loadBalancerVIPs, err := ovn.getLoadBalancerVIPs(loadBalancer)
			if err != nil {
				klog.Errorf("failed to get load-balancer vips for %s (%v)", loadBalancer, err)
				continue
			}
			for vip := range loadBalancerVIPs {
				_, port, err := net.SplitHostPort(vip)
				if err != nil {
					// In a OVN load-balancer, we should always have vip:port.
					// In the unlikely event that it is not the case, skip it.
					klog.Errorf("failed to split %s to vip and port (%v)", vip, err)
					continue
				}

				if !stringSliceMembership(nodeportServices[protocol], port) && !stringSliceMembership(lbServices[protocol], vip) {
					klog.V(5).Infof("Deleting stale nodeport vip %s in loadbalancer %s", vip, loadBalancer)
					if err := ovn.deleteLoadBalancerVIP(loadBalancer, vip); err != nil {
						klog.Error(err)
					}
				}
			}
		}
	}
}

func (ovn *Controller) createService(service *kapi.Service) error {
	klog.V(5).Infof("Creating service %s", service.Name)
	if !util.IsClusterIPSet(service) {
		klog.V(5).Infof("Skipping service create: No cluster IP for service %s found", service.Name)
		return nil
	} else if len(service.Spec.Ports) == 0 {
		klog.V(5).Info("Skipping service create: No Ports specified for service")
		return nil
	}

	// We can end up in a situation where the endpoint creation is triggered before the service creation,
	// we should not be creating the reject ACLs if this endpoint exists, because that would result in an unreachable service
	// eventough the endpoint exists.
	// NOTE: we can also end up in a situation where a service matching no pods is created. Such a service still has an endpoint, but with no subsets.
	// make sure to treat that service as an ACL reject.
	ep, err := ovn.watchFactory.GetEndpoint(service.Namespace, service.Name)
	if err == nil {
		if len(ep.Subsets) > 0 {
			klog.V(5).Infof("service: %s has endpoint, will create loadbalancer VIPs", service.Name)
		} else {
			klog.V(5).Infof("service: %s has empty endpoint", service.Name)
			ep = nil
		}
	}

	for _, svcPort := range service.Spec.Ports {
		var port int32
		if util.ServiceTypeHasNodePort(service) {
			port = svcPort.NodePort
		} else {
			port = svcPort.Port
		}

		if err := util.ValidatePort(svcPort.Protocol, port); err != nil {
			klog.Errorf("error validating port %s: %v", svcPort.Name, err)
			continue
		}

		if !ovn.SCTPSupport && svcPort.Protocol == kapi.ProtocolSCTP {
			ref, err := reference.GetReference(scheme.Scheme, service)
			if err != nil {
				klog.Errorf("Could not get reference for pod %v: %v\n", service.Name, err)
			} else {
				ovn.recorder.Event(ref, kapi.EventTypeWarning, "Unsupported protocol error", "SCTP protocol is unsupported by this version of OVN")
			}
			return fmt.Errorf("invalid service port %s: SCTP is unsupported by this version of OVN", svcPort.Name)
		}

		if util.ServiceTypeHasNodePort(service) {
			// Each gateway has a separate load-balancer for N/S traffic

			physicalGateways, _, err := ovn.getOvnGateways()
			if err != nil {
				return err
			}

			for _, physicalGateway := range physicalGateways {
				loadBalancer, err := ovn.getGatewayLoadBalancer(physicalGateway, svcPort.Protocol)
				if err != nil {
					klog.Errorf("physical gateway %s does not have load_balancer (%v)", physicalGateway, err)
					continue
				}
				physicalIPs, err := ovn.getGatewayPhysicalIPs(physicalGateway)
				if err != nil {
					klog.Errorf("physical gateway %s does not have physical ip (%v)", physicalGateway, err)
					continue
				}
				for _, physicalIP := range physicalIPs {
					// With the physical_ip:port as the VIP, add an entry in
					// 'load_balancer'.
					vip := util.JoinHostPortInt32(physicalIP, port)
					// Skip creating LB if endpoints watcher already did it
					if _, hasEps := ovn.getServiceLBInfo(loadBalancer, vip); hasEps {
						klog.V(5).Infof("Load Balancer already configured for %s, %s", loadBalancer, vip)
					} else if ep != nil {
						if err := ovn.AddEndpoints(ep); err != nil {
							return err
						}
					} else if ovn.svcQualifiesForReject(service) {
						aclUUID, err := ovn.createLoadBalancerRejectACL(loadBalancer, physicalIP, port, svcPort.Protocol)
						if err != nil {
							return fmt.Errorf("failed to create service ACL: %v", err)
						}
						klog.V(5).Infof("Service Reject ACL created for physical gateway: %s", aclUUID)
					}
				}
			}
		}
		if util.ServiceTypeHasClusterIP(service) {
			loadBalancer, err := ovn.getLoadBalancer(svcPort.Protocol)
			if err != nil {
				klog.Errorf("Failed to get load-balancer for %s (%v)", svcPort.Protocol, err)
				break
			}
			if ovn.svcQualifiesForReject(service) {
				vip := util.JoinHostPortInt32(service.Spec.ClusterIP, svcPort.Port)
				// Skip creating LB if endpoints watcher already did it
				if _, hasEps := ovn.getServiceLBInfo(loadBalancer, vip); hasEps {
					klog.V(5).Infof("Load Balancer already configured for %s, %s", loadBalancer, vip)
				} else if ep != nil {
					if err := ovn.AddEndpoints(ep); err != nil {
						return err
					}
				} else {
					aclUUID, err := ovn.createLoadBalancerRejectACL(loadBalancer, service.Spec.ClusterIP,
						svcPort.Port, svcPort.Protocol)
					if err != nil {
						return fmt.Errorf("failed to create service ACL: %v", err)
					}
					klog.V(5).Infof("Service Reject ACL created for cluster IP: %s", aclUUID)
				}
				for _, extIP := range service.Spec.ExternalIPs {
					gateways, _, err := ovn.getOvnGateways()
					if err != nil {
						return err
					}
					for _, gateway := range gateways {
						loadBalancer, err := ovn.getGatewayLoadBalancer(gateway, svcPort.Protocol)
						if err != nil {
							klog.Errorf("physical gateway %s does not have load_balancer (%v)", gateway, err)
							continue
						}
						vip := util.JoinHostPortInt32(extIP, svcPort.Port)
						// Skip creating LB if endpoints watcher already did it
						if _, hasEps := ovn.getServiceLBInfo(loadBalancer, vip); hasEps {
							klog.V(5).Infof("Load Balancer already configured for %s, %s", loadBalancer, vip)
						} else {
							aclUUID, err := ovn.createLoadBalancerRejectACL(loadBalancer, extIP, svcPort.Port, svcPort.Protocol)
							if err != nil {
								return fmt.Errorf("failed to create service ACL for external IP")
							}
							klog.V(5).Infof("Service Reject ACL created for external IP: %s", aclUUID)
						}
					}
				}
			}
		}
	}
	return nil
}

func (ovn *Controller) updateService(oldSvc, newSvc *kapi.Service) error {
	if reflect.DeepEqual(newSvc.Spec.Ports, oldSvc.Spec.Ports) &&
		reflect.DeepEqual(newSvc.Spec.ExternalIPs, oldSvc.Spec.ExternalIPs) &&
		reflect.DeepEqual(newSvc.Spec.ClusterIP, oldSvc.Spec.ClusterIP) &&
		reflect.DeepEqual(newSvc.Spec.Type, oldSvc.Spec.Type) {
		klog.V(5).Infof("skipping service update for: %s as change does not apply to any of .Spec.Ports, .Spec.ExternalIP, .Spec.ClusterIP, .Spec.Type", newSvc.Name)
		return nil
	}

	klog.V(5).Infof("updating service from: %v to: %v", oldSvc, newSvc)

	ovn.deleteService(oldSvc)
	return ovn.createService(newSvc)
}

func (ovn *Controller) deleteService(service *kapi.Service) {
	if !util.IsClusterIPSet(service) {
		return
	}
	for _, svcPort := range service.Spec.Ports {
		var port int32
		if util.ServiceTypeHasNodePort(service) {
			port = svcPort.NodePort
		} else {
			port = svcPort.Port
		}

		if err := util.ValidatePort(svcPort.Protocol, port); err != nil {
			klog.Errorf("Skipping delete for service port %s: %v", svcPort.Name, err)
			continue
		}

		if util.ServiceTypeHasNodePort(service) {
			// Delete the 'NodePort' service from a load-balancer instantiated in gateways.
			ovn.deleteGatewayVIPs(svcPort.Protocol, port)
		}
		if util.ServiceTypeHasClusterIP(service) {
			loadBalancer, err := ovn.getLoadBalancer(svcPort.Protocol)
			if err != nil {
				klog.Errorf("Failed to get load-balancer for %s (%v)", svcPort.Protocol, err)
				continue
			}
			vip := util.JoinHostPortInt32(service.Spec.ClusterIP, svcPort.Port)
			if err := ovn.deleteLoadBalancerVIP(loadBalancer, vip); err != nil {
				klog.Error(err)
			}
			if err := ovn.deleteExternalVIPs(service, svcPort); err != nil {
				klog.Error(err)
			}
		}
	}
}

// svcQualifiesForReject determines if a service should have a reject ACL on it when it has no endpoints
// The reject ACL is only applied to terminate incoming connections immediately when idling is not used
// or OVNEmptyLbEvents are not enabled. When idilng or empty LB events are enabled, we want to ensure we
// receive these packets and not reject them.
func (ovn *Controller) svcQualifiesForReject(service *kapi.Service) bool {
	_, ok := service.Annotations[OvnServiceIdledAt]
	return !(config.Kubernetes.OVNEmptyLbEvents && ok)
}
