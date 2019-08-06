package cni

// contains code for cnishim - one that gets called as the cni Plugin
// This does not do the real cni work. This is just the client to the cniserver
// that does the real work.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/sirupsen/logrus"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
)

// Plugin is the structure to hold the endpoint information and the corresponding
// functions to use it
type Plugin struct {
	socketPath string
	configPath string
}

// NewCNIPlugin creates the internal Plugin object
func NewCNIPlugin(socketPath, configPath string) *Plugin {
	if len(socketPath) == 0 {
		socketPath = serverSocketPath
	}
	if len(configPath) == 0 {
		configPath = serverConfigFilePath
	}
	return &Plugin{socketPath: socketPath, configPath: configPath}
}

// Create and fill a Request with this Plugin's environment and stdin which
// contain the CNI variables and configuration
func newCNIRequest(args *skel.CmdArgs) *Request {
	envMap := make(map[string]string)
	for _, item := range os.Environ() {
		idx := strings.Index(item, "=")
		if idx > 0 {
			envMap[strings.TrimSpace(item[:idx])] = item[idx+1:]
		}
	}

	return &Request{
		Env:    envMap,
		Config: args.StdinData,
	}
}

// Send a CNI request to the CNI server via JSON + HTTP over a root-owned unix socket,
// and return the result
func (p *Plugin) doCNI(url string, req *Request) ([]byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CNI request %v: %v", req, err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(proto, addr string) (net.Conn, error) {
				var conn net.Conn
				if runtime.GOOS != "windows" {
					conn, err = net.Dial("unix", p.socketPath)
				} else {
					conn, err = net.Dial("tcp", serverTCPAddress)
				}
				return conn, err
			},
		},
	}

	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to send CNI request: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read CNI result: %v", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("CNI request failed with status %v: '%s'", resp.StatusCode, string(body))
	}

	return body, nil
}

// CmdAdd is the callback for 'add' cni calls from skel
func (p *Plugin) CmdAdd(args *skel.CmdArgs) error {

	// read the config stdin args to obtain cniVersion
	conf, err := config.ReadCNIConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("invalid stdin args")
	}

	req := newCNIRequest(args)

	body, err := p.doCNI("http://dummy/", req)
	if err != nil {
		return err
	}
	cniconfig, _ := readConfig(p.configPath)
	if cniconfig != nil && cniconfig.HostIFCfg {
		pr, _ := cniRequestToPodRequest(req)
		ovnresult := &OvnResult{}
		if err := json.Unmarshal(body, ovnresult); err != nil {
			return fmt.Errorf("failed to unmarshal response '%s': %v", string(body), err)
		}
		var interfacesArray []*current.Interface
		ipAddress := ovnresult.IPAddress
		gatewayIP := ovnresult.GatewayIP
		interfacesArray, err = pr.ConfigureInterface(pr.PodNamespace, pr.PodName, ovnresult.MacAddress, ipAddress, gatewayIP, cniconfig.MTU, ovnresult.Ingress, ovnresult.Egress)
		if err != nil {
			logrus.Errorf("Failed to configure interface in pod: %v", err)
			return nil
		}

		// Build the result structure to pass back to the runtime
		addr, addrNet, err := net.ParseCIDR(ipAddress)
		if err != nil {
			logrus.Errorf("failed to parse IP address %q: %v", ipAddress, err)
			return nil
		}
		ipVersion := "6"
		if addr.To4() != nil {
			ipVersion = "4"
		}
		result := &current.Result{
			Interfaces: interfacesArray,
			IPs: []*current.IPConfig{
				{
					Version:   ipVersion,
					Interface: current.Int(1),
					Address:   net.IPNet{IP: addr, Mask: addrNet.Mask},
					Gateway:   net.ParseIP(gatewayIP),
				},
			},
		}
		return types.PrintResult(result, conf.CNIVersion)
	}

	result, err := current.NewResult(body)
	if err != nil {
		return fmt.Errorf("failed to unmarshal response '%s': %v", string(body), err)
	}

	return types.PrintResult(result, conf.CNIVersion)
}

// CmdDel is the callback for 'teardown' cni calls from skel
func (p *Plugin) CmdDel(args *skel.CmdArgs) error {
	_, err := p.doCNI("http://dummy/", newCNIRequest(args))
	return err
}

func readConfig(configPath string) (*Config, error) {
	bytes, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var config Config
	if err = json.Unmarshal(bytes, &config); err != nil {
		return nil, fmt.Errorf("could not parse config file %q: %v", configPath, err)
	}
	return &config, nil
}
