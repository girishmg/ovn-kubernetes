package node

import (
	kapi "k8s.io/api/core/v1"
)

func initNodePortIptableChain() error {
	return nil
}

func deleteNodePortIptableChain() {
}

func addSharedGatewayIptRules(service *kapi.Service) {
}

func delSharedGatewayIptRules(service *kapi.Service) {
}
