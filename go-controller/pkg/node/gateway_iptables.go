// +build linux

package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

type iptRule struct {
	table    string
	chain    string
	args     []string
	protocol iptables.Protocol
}

func addIptRules(rules []iptRule) error {
	for _, r := range rules {
		ipt, _ := util.GetIPTablesHelper(r.protocol)
		if err := ensureChain(ipt, r.table, r.chain); err != nil {
			return fmt.Errorf("failed to ensure %s/%s: %v", r.table, r.chain, err)
		}
		exists, err := ipt.Exists(r.table, r.chain, r.args...)
		if !exists && err == nil {
			err = ipt.Insert(r.table, r.chain, 1, r.args...)
		}
		if err != nil {
			return fmt.Errorf("failed to add iptables %s/%s rule %q: %v",
				r.table, r.chain, strings.Join(r.args, " "), err)
		}
	}
	return nil
}

func delIptRules(rules []iptRule) error {
	for _, r := range rules {
		ipt, _ := util.GetIPTablesHelper(r.protocol)
		err := ipt.Delete(r.table, r.chain, r.args...)
		if err != nil {
			return fmt.Errorf("failed to delete iptables %s/%s rule %q: %v", r.table, r.chain,
				strings.Join(r.args, " "), err)
		}
	}
	return nil
}

func ensureChain(ipt util.IPTablesHelper, table, chain string) error {
	chains, err := ipt.ListChains(table)
	if err != nil {
		return fmt.Errorf("failed to list iptables chains: %v", err)
	}
	for _, ch := range chains {
		if ch == chain {
			return nil
		}
	}
	return ipt.NewChain(table, chain)
}

func initSharedGatewayIPTables() error {
	rules := make([]iptRule, 0)
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		for _, proto := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
			ipt, err := util.GetIPTablesHelper(proto)
			if err != nil {
				return err
			}
			_ = ipt.ClearChain("nat", chain)
			_ = ipt.ClearChain("filter", chain)

			rules = append(rules, iptRule{
				table:    "nat",
				chain:    "OUTPUT",
				args:     []string{"-j", chain},
				protocol: proto,
			})
			rules = append(rules, iptRule{
				table:    "nat",
				chain:    "PREROUTING",
				args:     []string{"-j", chain},
				protocol: proto,
			})
			rules = append(rules, iptRule{
				table:    "filter",
				chain:    "OUTPUT",
				args:     []string{"-j", chain},
				protocol: proto,
			})
			rules = append(rules, iptRule{
				table:    "filter",
				chain:    "FORWARD",
				args:     []string{"-j", chain},
				protocol: proto,
			})
		}
	}
	if err := addIptRules(rules); err != nil {
		return fmt.Errorf("failed to add iptable rules %v: %v", rules, err)
	}
	return nil
}

func cleanupSharedGatewayIPTChains() {
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		for _, proto := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
			ipt, err := util.GetIPTablesHelper(proto)
			if err != nil {
				return
			}
			_ = ipt.ClearChain("nat", chain)
			_ = ipt.ClearChain("filter", chain)
			_ = ipt.DeleteChain("nat", chain)
			_ = ipt.DeleteChain("filter", chain)
		}
	}
}

func initLocalGatewayIPTables() error {
	rules := make([]iptRule, 0)
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		for _, proto := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
			ipt, _ := util.GetIPTablesHelper(proto)
			if err := ipt.NewChain("nat", chain); err != nil {
				klog.Warning(err)
			}
			if err := ipt.NewChain("filter", chain); err != nil {
				klog.Warning(err)
			}
			rules = append(rules, iptRule{
				table:    "nat",
				chain:    "PREROUTING",
				args:     []string{"-j", chain},
				protocol: proto,
			})
			rules = append(rules, iptRule{
				table:    "nat",
				chain:    "OUTPUT",
				args:     []string{"-j", chain},
				protocol: proto,
			})
			rules = append(rules, iptRule{
				table:    "filter",
				chain:    "FORWARD",
				args:     []string{"-j", chain},
				protocol: proto,
			})
		}
	}
	if err := addIptRules(rules); err != nil {
		return err
	}
	return nil
}

func initLocalGatewayNATRules(ifname string, ip net.IP) error {
	// Allow packets to/from the gateway interface in case defaults deny
	var protocol iptables.Protocol
	if utilnet.IsIPv6(ip) {
		protocol = iptables.ProtocolIPv6
	} else {
		protocol = iptables.ProtocolIPv4
	}
	rules := make([]iptRule, 0)
	rules = append(rules, iptRule{
		table:    "filter",
		chain:    "FORWARD",
		args:     []string{"-i", ifname, "-j", "ACCEPT"},
		protocol: protocol,
	})
	rules = append(rules, iptRule{
		table: "filter",
		chain: "FORWARD",
		args: []string{"-o", ifname, "-m", "conntrack", "--ctstate",
			"RELATED,ESTABLISHED", "-j", "ACCEPT"},
		protocol: protocol,
	})
	rules = append(rules, iptRule{
		table:    "filter",
		chain:    "INPUT",
		args:     []string{"-i", ifname, "-m", "comment", "--comment", "from OVN to localhost", "-j", "ACCEPT"},
		protocol: protocol,
	})

	// NAT for the interface
	rules = append(rules, iptRule{
		table:    "nat",
		chain:    "POSTROUTING",
		args:     []string{"-s", ip.String(), "-j", "MASQUERADE"},
		protocol: protocol,
	})
	return addIptRules(rules)
}

func isFoundInKeepRule(ipTableRule string, keepIPTRules []iptRule, proto iptables.Protocol) bool {
	// iptables returns the chain name when listing all rules, ex: 'iptables -t nat -S OVN-KUBE-NODEPORT'
	// returns, among all other: '-N OVN-KUBE-NODEPORT', we need to skip those so that we don't try to delete them.
	if strings.HasPrefix(ipTableRule, "-P") || strings.HasPrefix(ipTableRule, "-N") {
		return true
	}
	for _, keepIPTRule := range keepIPTRules {
		if keepIPTRule.protocol == proto && ipTableRule == strings.Join(keepIPTRule.args, " ") {
			return true
		}
	}
	return false
}

func removeStaleIPTRules(table, chain string, keepIPTRules []iptRule) {
	for _, proto := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
		ipt, _ := util.GetIPTablesHelper(proto)
		if ipTableRules, err := ipt.List(table, chain); err == nil {
			for _, ipTableRule := range ipTableRules {
				if !isFoundInKeepRule(ipTableRule, keepIPTRules, proto) {
					klog.V(5).Infof("deleting stale iptables rule: table: %s, chain: %s, rule: %s", table, chain, ipTableRule)
					if err := ipt.Delete(table, chain, strings.Fields(ipTableRule)...); err != nil {
						klog.Errorf("error deleting stale iptables rule: table: %s, chain: %s, rule: %v, err: %v", table, chain, ipTableRule, err)
					}
				}
			}
		}
	}
}

func getGatewayIptRules(service *kapi.Service, nodePortTargetIP string) []iptRule {
	rules := make([]iptRule, 0)
	for _, svcPort := range service.Spec.Ports {
		if util.ServiceTypeHasNodePort(service) {
			err := util.ValidatePort(svcPort.Protocol, svcPort.NodePort)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service NodePort: %v", svcPort.Name, err)
				continue
			}
			err = util.ValidatePort(svcPort.Protocol, svcPort.Port)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service port %v", svcPort.Name, err)
				continue
			}
			rules = append(rules, nodePortIPTRules(svcPort, nodePortTargetIP, svcPort.Port)...)
		}
		for _, externalIP := range service.Spec.ExternalIPs {
			err := util.ValidatePort(svcPort.Protocol, svcPort.Port)
			if err != nil {
				klog.Errorf("Skipping service: %s, invalid service port %v", svcPort.Name, err)
				continue
			}
			rules = append(rules, externalIPTRules(svcPort, externalIP, service.Spec.ClusterIP)...)
		}

	}
	return rules
}

func nodePortIPTRules(svcPort kapi.ServicePort, gatewayIP string, targetPort int32) []iptRule {
	var protocol iptables.Protocol
	if utilnet.IsIPv6String(gatewayIP) {
		protocol = iptables.ProtocolIPv6
	} else {
		protocol = iptables.ProtocolIPv4
	}
	return []iptRule{
		{
			table: "nat",
			chain: iptableNodePortChain,
			args: []string{
				"-p", string(svcPort.Protocol),
				"--dport", fmt.Sprintf("%d", svcPort.NodePort),
				"-j", "DNAT",
				"--to-destination", net.JoinHostPort(gatewayIP, fmt.Sprintf("%d", targetPort)),
			},
			protocol: protocol,
		},
		{
			table: "filter",
			chain: iptableNodePortChain,
			args: []string{
				"-p", string(svcPort.Protocol),
				"--dport", fmt.Sprintf("%d", svcPort.NodePort),
				"-j", "ACCEPT",
			},
			protocol: protocol,
		},
	}
}

func externalIPTRules(svcPort kapi.ServicePort, externalIP, dstIP string) []iptRule {
	var protocol iptables.Protocol
	if utilnet.IsIPv6String(externalIP) {
		protocol = iptables.ProtocolIPv6
	} else {
		protocol = iptables.ProtocolIPv4
	}
	return []iptRule{
		{
			table: "nat",
			chain: iptableExternalIPChain,
			args: []string{
				"-p", string(svcPort.Protocol),
				"-d", externalIP,
				"--dport", fmt.Sprintf("%v", svcPort.Port),
				"-j", "DNAT",
				"--to-destination", net.JoinHostPort(dstIP, fmt.Sprintf("%v", svcPort.Port)),
			},
			protocol: protocol,
		},
		{
			table: "filter",
			chain: iptableExternalIPChain,
			args: []string{
				"-p", string(svcPort.Protocol),
				"-d", externalIP,
				"--dport", fmt.Sprintf("%v", svcPort.Port),
				"-j", "ACCEPT",
			},
			protocol: protocol,
		},
	}
}
