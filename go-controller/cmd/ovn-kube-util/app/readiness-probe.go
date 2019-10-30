package app

import (
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli"
	kexec "k8s.io/utils/exec"
)

// BridgesToNicCommand removes a NIC interface from OVS bridge and deletes the bridge
var ReadinessProbeCommand = cli.Command{
	Name:  "readiness-probe",
	Usage: "check readiness of the specified target container",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "target, t",
			Usage: "target of the readiness probe",
		},
	},
	Action: func(ctx *cli.Context) error {
		var ret, target string
		var err error

		target = ctx.String("target")
		if err = util.SetExec(kexec.New()); err != nil {
			return err
		}

		if target == "ovn-controller" {
			ret, _, err = util.RunOVSAppctlWithTimeout(5, "-t", target, "connection-status")
			if err != nil {
				return fmt.Errorf("%s is not ready: (%v)", target, err)
			} else if ret != "connected" {
				return fmt.Errorf("%s is not ready, status: (%s)", target, ret)
			}
			_, _, err = util.RunOVSVsctlWithTimeout(5, "show")
			if err != nil {
				return fmt.Errorf("Open vSwitch database server is not ready: %v", err)
			}
			_, _, err = util.RunOVSAppctlWithTimeout(5, "-t", "ovs-vswitchd", "ofproto/list")
			if err != nil {
				return fmt.Errorf("Open vSwitch daemon is not ready: %v", err)
			}
			return nil
		} else if target == "ovnnb_db" || target == "ovnsb_db" {
			_, _, err = util.RunOVSAppctlWithTimeout(5, "-t", "/var/run/openvswitch/"+target+".ctl", "version")
			if err != nil {
				return fmt.Errorf("%s is not ready: (%v)", target, err)
			}
			if target == "ovnnb_db" {
				ret, _, err = util.RunOVNNbctlWithTimeout(5, "--data=bare", "--no-heading", "--columns=target",
					"find", "connection", "target!=_")
			} else {
				ret, _, err = util.RunOVNSbctlWithTimeout(5, "--data=bare", "--no-heading", "--columns=target",
					"find", "connection", "target!=_")
			}
			if err != nil {
				return fmt.Errorf("%s is not ready: (%v)", target, err)
			}
			connections := strings.Fields(ret)
			for _, connection := range connections {
				if strings.HasPrefix(connection, "ptcp") ||  strings.HasPrefix(connection, "pssl") {
					return nil
				}
			}
			return fmt.Errorf("%v does not listen on any connections, connection: %v", connections)
		} else if target == "ovn-northd" || target == "ovn-nbctl" {
			_, _, err = util.RunOVSAppctlWithTimeout(5, "-t", target, "version")
			if err != nil {
				return fmt.Errorf("%s is not ready: (%v)", target, err)
			}
		} else if target == "ovs-daemons" {
			_, _, err = util.RunOVSVsctlWithTimeout(5, "show")
			if err != nil {
				return fmt.Errorf("Open vSwitch database server is not ready: %v", err)
			}
			_, _, err = util.RunOVSAppctlWithTimeout(5, "-t", "ovs-vswitchd", "ofproto/list")
			if err != nil {
				return fmt.Errorf("Open vSwitch daemon is not ready: %v", err)
			}
		} else {
			return fmt.Errorf("%v is not a supported target", target)
		}
		return nil
	},
}