package util

import (
	"fmt"
	"strings"
	"encoding/json"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/cert"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
)

// NewClientset creates a Kubernetes clientset from either a kubeconfig,
// TLS properties, or an apiserver URL
func NewClientset(conf *config.KubernetesConfig) (*kubernetes.Clientset, error) {
	var kconfig *rest.Config
	var err error

	if conf.Kubeconfig != "" {
		// uses the current context in kubeconfig
		kconfig, err = clientcmd.BuildConfigFromFlags("", conf.Kubeconfig)
	} else if strings.HasPrefix(conf.APIServer, "https") {
		if conf.APIServer == "" || conf.Token == "" {
			return nil, fmt.Errorf("TLS-secured apiservers require token and CA certificate")
		}
		kconfig = &rest.Config{
			Host:        conf.APIServer,
			BearerToken: conf.Token,
		}
		if conf.CACert != "" {
			if _, err := cert.NewPool(conf.CACert); err != nil {
				return nil, err
			}
			kconfig.TLSClientConfig = rest.TLSClientConfig{CAFile: conf.CACert}
		}
	} else if strings.HasPrefix(conf.APIServer, "http") {
		kconfig, err = clientcmd.BuildConfigFromFlags(conf.APIServer, "")
	} else {
		// Assume we are running from a container managed by kubernetes
		// and read the apiserver address and tokens from the
		// container's environment.
		kconfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(kconfig)
}

// IsClusterIPSet checks if the service is an headless service or not
func IsClusterIPSet(service *kapi.Service) bool {
	return service.Spec.ClusterIP != kapi.ClusterIPNone && service.Spec.ClusterIP != ""
}

// ServiceTypeHasClusterIP checks if the service has an associated ClusterIP or not
func ServiceTypeHasClusterIP(service *kapi.Service) bool {
	return service.Spec.Type == kapi.ServiceTypeClusterIP || service.Spec.Type == kapi.ServiceTypeNodePort || service.Spec.Type == kapi.ServiceTypeLoadBalancer
}

// ServiceTypeHasNodePort checks if the service has an associated NodePort or not
func ServiceTypeHasNodePort(service *kapi.Service) bool {
	return service.Spec.Type == kapi.ServiceTypeNodePort || service.Spec.Type == kapi.ServiceTypeLoadBalancer
}

const (
	defaultNetAnnot = "v1.multus-cni.io/default-network"
)

// GetPodCustomConfig return the default network attachment selection annotation that could
// include custom IP/MAC.
//
// This function is a simplified version of parsePodNetworkAnnotation() function in multus-cni, need to revisit
// once there is a library function that we can share.
//
// Also, OVN CNI today makes a simple assumption that all the Pods' default network is OVN, as the result, we make
// the same assumption and check the custom configuration of the network attachment selection annotation defined in
// default-network only
func GetPodCustomConfig(pod *kapi.Pod) (*types.NetworkSelectionElement, error) {
	var netAnnot string
	var networks []*types.NetworkSelectionElement

	netAnnot, _ = pod.Annotations[defaultNetAnnot]
	if netAnnot == "" {
		return nil, fmt.Errorf("Pod default network annotation is not defined")
	}

	// it is possible the default network is defined in the form of comma-delimited list of
	// network attachment object name (i.e. list of <namespace>/<network name>@<ifname>), but
	// we are only interested in the NetworkSelectionElement form that custom MAC/IP can be defined
	if strings.IndexAny(netAnnot, "[{\"") >= 0 {
		if err := json.Unmarshal([]byte(netAnnot), &networks); err != nil {
			return nil, fmt.Errorf("failed to parse pod Network Attachment Selection Annotation JSON format: %v", err)
		}
	} else {
		return nil, fmt.Errorf("no custom config is specified in the Pod default network annotation: %s", netAnnot)
	}

	if len(networks) == 1 {
		return networks[0], nil
	}
	return nil, fmt.Errorf("no or more than one default network is specified: %s", netAnnot)
}
