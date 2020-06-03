package node

import (
	"fmt"
	"net"
	"reflect"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
)

var port localPort

type activeSocket interface {
	Close() error
}

type localPort interface {
	open(port int32, protocol kapi.Protocol, svc *kapi.Service) error
	close(port int32, svc *kapi.Service) error
}

type portClaimWatcher struct {
	recorder          record.EventRecorder
	activeSocketsLock sync.Mutex
	activeSockets     map[int32]activeSocket
}

func newPortClaimWatcher(recorder record.EventRecorder) localPort {
	return &portClaimWatcher{
		recorder:          recorder,
		activeSocketsLock: sync.Mutex{},
		activeSockets:     make(map[int32]activeSocket),
	}
}

func initPortClaimWatcher(recorder record.EventRecorder, wf *factory.WatchFactory) error {
	port = newPortClaimWatcher(recorder)
	_, err := wf.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			if errors := addServicePortClaim(svc); len(errors) > 0 {
				for _, err := range errors {
					klog.Error(err)
				}
			}
		},
		UpdateFunc: func(old, new interface{}) {
			oldSvc := old.(*kapi.Service)
			newSvc := new.(*kapi.Service)
			if errors := updateServicePortClaim(oldSvc, newSvc); len(errors) > 0 {
				for _, err := range errors {
					klog.Error(err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			if errors := deleteServicePortClaim(svc); len(errors) > 0 {
				for _, err := range errors {
					klog.Error(err)
				}
			}
		},
	}, nil)
	return err
}

func addServicePortClaim(svc *kapi.Service) []error {
	errors := []error{}
	if !util.ServiceTypeHasNodePort(svc) && len(svc.Spec.ExternalIPs) == 0 {
		return errors
	}
	for _, svcPort := range svc.Spec.Ports {
		if util.ServiceTypeHasNodePort(svc) {
			if err := util.ValidatePort(svcPort.Protocol, svcPort.NodePort); err != nil {
				errors = append(errors, fmt.Errorf("invalid service port %s, err: %v", svc.Name, err))
				continue
			}
			if err := port.open(svcPort.NodePort, svcPort.Protocol, svc); err != nil {
				errors = append(errors, err)
			}
		}
		if len(svc.Spec.ExternalIPs) > 0 {
			if err := util.ValidatePort(svcPort.Protocol, svcPort.Port); err != nil {
				errors = append(errors, fmt.Errorf("invalid service port %s, err: %v", svc.Name, err))
				continue
			}
			if err := port.open(svcPort.Port, svcPort.Protocol, svc); err != nil {
				errors = append(errors, err)
			}
		}
	}
	return errors
}

func deleteServicePortClaim(svc *kapi.Service) []error {
	errors := []error{}
	if !util.ServiceTypeHasNodePort(svc) && len(svc.Spec.ExternalIPs) == 0 {
		return errors
	}
	for _, svcPort := range svc.Spec.Ports {
		if util.ServiceTypeHasNodePort(svc) {
			if err := util.ValidatePort(svcPort.Protocol, svcPort.NodePort); err != nil {
				errors = append(errors, fmt.Errorf("invalid service port %s, err: %v", svc.Name, err))
				continue
			}
			if err := port.close(svcPort.NodePort, svc); err != nil {
				errors = append(errors, err)
			}
		}
		if len(svc.Spec.ExternalIPs) > 0 {
			if err := util.ValidatePort(svcPort.Protocol, svcPort.Port); err != nil {
				errors = append(errors, fmt.Errorf("invalid service port %s, err: %v", svc.Name, err))
				continue
			}
			if err := port.close(svcPort.Port, svc); err != nil {
				errors = append(errors, err)
			}
		}
	}
	return errors
}

func updateServicePortClaim(oldSvc, newSvc *kapi.Service) []error {
	if reflect.DeepEqual(oldSvc.Spec.ExternalIPs, newSvc.Spec.ExternalIPs) && reflect.DeepEqual(oldSvc.Spec.Ports, newSvc.Spec.Ports) {
		return nil
	}
	errors := []error{}
	errors = append(errors, deleteServicePortClaim(oldSvc)...)
	errors = append(errors, addServicePortClaim(newSvc)...)
	return errors
}

func (p *portClaimWatcher) open(port int32, protocol kapi.Protocol, svc *kapi.Service) error {
	klog.V(5).Infof("opening socket for service: %s and port: %v", svc.Name, port)
	var socket activeSocket
	var socketError error
	switch protocol {
	case kapi.ProtocolTCP:
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			socketError = err
			break
		}
		socket = listener
	case kapi.ProtocolUDP:
		addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", port))
		if err != nil {
			socketError = err
			break
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			socketError = err
			break
		}
		socket = conn
	case kapi.ProtocolSCTP:
		// Do not open ports for SCTP, ref: https://github.com/kubernetes/enhancements/blob/master/keps/sig-network/0015-20180614-SCTP-support.md#the-solution-in-the-kubernetes-sctp-support-implementation
		return nil
	default:
		socketError = fmt.Errorf("unknown protocol %q", protocol)
	}
	if socketError != nil {
		p.emitPortClaimEvent(svc, port, socketError)
		return socketError
	}
	p.activeSocketsLock.Lock()
	p.activeSockets[port] = socket
	p.activeSocketsLock.Unlock()
	return nil
}

func (p *portClaimWatcher) close(port int32, svc *kapi.Service) error {
	p.activeSocketsLock.Lock()
	defer p.activeSocketsLock.Unlock()
	klog.V(5).Infof("closing socket claimed for service: %s and port: %v", svc.Name, port)
	if socket, exists := p.activeSockets[port]; exists {
		if err := socket.Close(); err != nil {
			return fmt.Errorf("error closing socket for svc: %s on port: %v, err: %v", svc.Name, port, err)
		}
		delete(p.activeSockets, port)
		return nil
	}
	return fmt.Errorf("error closing socket for svc: %s on port: %v, port was never opened...?", svc.Name, port)
}

func (p *portClaimWatcher) emitPortClaimEvent(svc *kapi.Service, port int32, err error) {
	serviceRef := kapi.ObjectReference{
		Kind:      "Service",
		Namespace: svc.Namespace,
		Name:      svc.Name,
	}
	p.recorder.Eventf(&serviceRef, kapi.EventTypeWarning, "PortClaim", "Service: %s specifies node local port: %v, but port cannot be bound to: %v", svc.Name, port, err)
	klog.Warningf("PortClaim for svc: %s on port: %v, err: %v", svc.Name, port, err)
}
