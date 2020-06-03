package node

import (
	kapi "k8s.io/api/core/v1"
	"net"
)

func initSharedGatewayIPTables() error {
	return nil
}

func cleanupSharedGatewayIPTChains() {
}

func addSharedGatewayIptRules(service *kapi.Service, nodeIP *net.IPNet) {
}

func delSharedGatewayIptRules(service *kapi.Service, nodeIP *net.IPNet) {
}

func syncSharedGatewayIptRules(services []interface{}) {
}

func setupLocalNodeAccessBridge(nodeName string, subnet *net.IPNet) error {
	return nil
}
