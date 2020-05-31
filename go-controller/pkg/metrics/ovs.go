package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog"
)

const (
	ovsVswitchd = "ovs-vswitchd"
)

var (
	ovsVersion string
)

// ovs Data Path Metrics
var metricOvsDpTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_total",
	Help:      "Represents total number of datapaths on the system.",
})

var metricOvsDp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp",
	Help: "A metric with a constant '1' value labeled by datapath " +
		"name present on the instance."},
	[]string{
		"datapath",
		"type",
	},
)

var metricOvsDpIfTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_if_total",
	Help:      "Represents the number of ports connected to the datapath."},
	[]string{
		"datapath",
	},
)

var metricOvsDpIf = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_if",
	Help: "A metric with a constant '1' value labeled by " +
		"datapath name, port name, port type and datapath port number."},
	[]string{
		"datapath",
		"port",
		"type",
		"ofPort",
	},
)

var metricOvsDpFlowsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_flows_total",
	Help:      "Represents the number of flows in datapath."},
	[]string{
		"datapath",
	},
)

var metricOvsDpFlowsLookupHit = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_flows_lookup_hit",
	Help: "Represents number of packets matching the existing flows " +
		"while processing incoming packets in the datapath."},
	[]string{
		"datapath",
	},
)

var metricOvsDpFlowsLookupMissed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_flows_lookup_missed",
	Help: "Represents the number of packets not matching any existing " +
		"flow  and require  user space processing."},
	[]string{
		"datapath",
	},
)

var metricOvsDpFlowsLookupLost = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_flows_lookup_lost",
	Help: "number of packets destined for user space process but " +
		"subsequently dropped before  reaching  userspace."},
	[]string{
		"datapath",
	},
)

var metricOvsDpPacketsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_packets_total",
	Help: "Represents the total number of packets datapath processed " +
		"which is the sum of hit and missed."},
	[]string{
		"datapath",
	},
)

var metricOvsdpMasksHit = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_masks_hit",
	Help:      "Represents the total number of masks visited for matching incoming packets.",
},
	[]string{
		"datapath",
	},
)

var metricOvsDpMasksTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_masks_total",
	Help:      "Represents the number of masks in a datapath."},
	[]string{
		"datapath",
	},
)

var metricOvsDpMasksHitRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "dp_masks_hit_ratio",
	Help: "Represents the average number of masks visited per packet " +
		"the  ratio between hit and total number of packets processed by the datapath."},
	[]string{
		"datapath",
	},
)

// ovs bridge statistics & attributes metrics
var metricOvsBridgeTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "bridge_total",
	Help:      "Represents total number of OVS bridges on the system.",
},
)

var metricOvsBridge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "bridge",
	Help: "A metric with a constant '1' value labeled by bridge name " +
		"present on the instance."},
	[]string{
		"bridge",
	},
)

var metricOvsBridgePortsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "bridge_ports_total",
	Help:      "Represents the number of OVS ports on the bridge."},
	[]string{
		"bridge",
	},
)

var metricOvsBridgeFlowsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "bridge_flows_total",
	Help:      "Represents the number of OpenFlow flows on the OVS bridge."},
	[]string{
		"bridge",
	},
)

// ovs memory metrics
var metricOvsHandlersTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "handlers_total",
	Help: "Represents the number of handlers thread. This thread reads upcalls from dpif, " +
		"forwards each upcall's packet and possibly sets up a kernel flow as a cache.",
})

var metricOvsRevalidatorsTotal = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "revalidators_total",
	Help: "Represents the number of revalidators thread. This thread processes datapath flows, " +
		"updates OpenFlow statistics, and updates or removes them if necessary.",
})

// ovs Hw offload metrics
var metricOvsHwOffload = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "hw_offload",
	Help: "Represents whether netdev flow offload to hardware is enabled " +
		"or not -- false(0) and true(1).",
})

var metricOvsTcPolicy = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: MetricOvsNamespace,
	Subsystem: MetricOvsSubsystemVswitchd,
	Name:      "tc_policy",
	Help: "Represents the policy used with HW offloading " +
		"-- none(0), skip_sw(1), and skip_hw(2)..",
})

func getOvsVersionInfo() {
	stdout, _, err := util.RunOVSVsctl("--version")
	if err == nil && strings.HasPrefix(stdout, "ovs-vsctl (Open vSwitch)") {
		ovsVersion = strings.Fields(stdout)[3]
	}
}

func parseMetricToFloat(name, value string) float64 {
	f64Value, err := strconv.ParseFloat(value, 64)
	if err != nil {
		klog.Errorf("Failed to parse %s value into float for metric %s :(%v)",
			value, name, err)
		return 0
	}
	return f64Value
}

// ovsDataPathLookupsMetrics obtains the ovs datapath
// (lookups: hit, missed, lost) metrics and updates them.
func ovsDataPathLookupsMetrics(output, dataPath string) {
	var dataPathPacketsTotal float64
	for _, field := range strings.Fields(output) {
		elem := strings.Split(field, ":")
		if len(elem) != 2 {
			continue
		}
		switch elem[0] {
		case "hit":
			value := parseMetricToFloat("dp_flows_lookup_hit", elem[1])
			dataPathPacketsTotal = dataPathPacketsTotal + value
			metricOvsDpFlowsLookupHit.WithLabelValues(dataPath).Set(value)
		case "missed":
			value := parseMetricToFloat("dp_flows_lookup_missed", elem[1])
			dataPathPacketsTotal = dataPathPacketsTotal + value
			metricOvsDpFlowsLookupMissed.WithLabelValues(dataPath).Set(value)
		case "lost":
			value := parseMetricToFloat("dp_flows_lookup_lost", elem[1])
			metricOvsDpFlowsLookupLost.WithLabelValues(dataPath).Set(value)
		}
	}
	metricOvsDpPacketsTotal.WithLabelValues(dataPath).Set(dataPathPacketsTotal)
}

// ovsDataPathMasksMetrics obatins ovs datapath masks metrics
// (masks :hit, total, hit/pkt) and updates them.
func ovsDataPathMasksMetrics(output, datapath string) {
	for _, field := range strings.Fields(output) {
		elem := strings.Split(field, ":")
		if len(elem) != 2 {
			continue
		}
		switch elem[0] {
		case "hit":
			value := parseMetricToFloat("dp_masks_hit", elem[1])
			metricOvsdpMasksHit.WithLabelValues(datapath).Set(value)
		case "total":
			value := parseMetricToFloat("dp_masks_total", elem[1])
			metricOvsDpMasksTotal.WithLabelValues(datapath).Set(value)
		case "hit/pkt":
			value := parseMetricToFloat("dp_masks_hit_ratio", elem[1])
			metricOvsDpMasksHitRatio.WithLabelValues(datapath).Set(value)
		}
	}
}

// ovsDataPathPortMetrics obtains the ovs datapath port metrics
// from ovs-appctl dpctl/show(portname, porttype, portnumber) and updates them.
func ovsDataPathPortMetrics(output, datapath string) {
	portFields := strings.Fields(output)
	portType := "system"
	if len(portFields) > 3 {
		portType = strings.Trim(portFields[3], "():")
	}

	portName := strings.TrimSpace(portFields[2])
	portNumber := strings.Trim(portFields[1], ":")
	metricOvsDpIf.WithLabelValues(datapath, portName, portType, portNumber).Set(1)
}

// getOvsDatapaths gives list of datapaths
// and updates the corresponding dataPath metrics
func getOvsDataPaths() (dataPathsList []string, err error) {
	var stdout, stderr string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic while parsing the "+
				"ovs-dpctl dump-dps output : %v", r)
		}
	}()

	stdout, stderr, err = util.RunOVSDpctl("dump-dps")
	if err != nil {
		return nil, fmt.Errorf("failed to get output of ovs-dpctl dump-dps "+
			"stderr(%s) :(%v)", stderr, err)
	}
	for _, kvPair := range strings.Split(stdout, "\n") {
		var dataPathType, dataPathName string
		output := strings.TrimSpace(kvPair)
		if strings.Contains(output, "@") {
			dataPath := strings.Split(output, "@")
			dataPathType, dataPathName = dataPath[0], dataPath[1]
		} else {
			return nil, fmt.Errorf("datapath %s is not of format Type@Name", output)
		}
		metricOvsDp.WithLabelValues(dataPathName, dataPathType).Set(1)
		dataPathsList = append(dataPathsList, dataPathName)
	}
	metricOvsDpTotal.Set(float64(len(dataPathsList)))
	return dataPathsList, nil
}

func setOvsDataPathMetrics(dataPaths []string) (err error) {
	var stdout, stderr, dataPathName string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic while parsing the ovs-dpctl "+
				"show %s output : %v", dataPathName, r)
		}
	}()

	if len(dataPaths) == 0 {
		klog.Infof("Currently, no datapath is present ")
	}

	for _, dataPathName = range dataPaths {
		stdout, stderr, err = util.RunOVSDpctl("show", dataPathName)
		if err != nil {
			return fmt.Errorf("failed to get datapath stats for %s "+
				"stderr(%s) :(%v)", dataPathName, stderr, err)
		}
		var dataPathPortCount float64
		for i, kvPair := range strings.Split(stdout, "\n") {
			if i <= 0 {
				continue
			}
			output := strings.TrimSpace(kvPair)
			if strings.HasPrefix(output, "lookups:") {
				ovsDataPathLookupsMetrics(output, dataPathName)
			} else if strings.HasPrefix(output, "masks:") {
				ovsDataPathMasksMetrics(output, dataPathName)
			} else if strings.HasPrefix(output, "port ") {
				ovsDataPathPortMetrics(output, dataPathName)
				dataPathPortCount++
			} else if strings.HasPrefix(output, "flows:") {
				flowFields := strings.Fields(output)
				value := parseMetricToFloat("dp_flows_total", flowFields[1])
				metricOvsDpFlowsTotal.WithLabelValues(dataPathName).Set(value)
			}
		}
		metricOvsDpIfTotal.WithLabelValues(dataPathName).Set(dataPathPortCount)
	}
	return nil
}

// ovsDataPathMetricsUpdate updates the ovs datapath metrics for every 30 sec
func ovsDataPathMetricsUpdate() {
	for {
		dataPaths, err := getOvsDataPaths()
		if err != nil {
			klog.Errorf("%s", err.Error())
			time.Sleep(30 * time.Second)
			continue
		}

		err = setOvsDataPathMetrics(dataPaths)
		if err != nil {
			klog.Errorf("%s", err.Error())
		}
		time.Sleep(30 * time.Second)
	}
}

// getOvsBridgeOpenFlowsCount returns the number of openflow flows
// in an ovs-bridge
func getOvsBridgeOpenFlowsCount(bridgeName string) float64 {
	stdout, stderr, err := util.RunOVSOfctl("-t", "5", "dump-aggregate", bridgeName)
	if err != nil {
		klog.Errorf("Failed to get flow count for %s, stderr(%s): (%v)",
			bridgeName, stderr, err)
		return 0
	}
	for _, kvPair := range strings.Fields(stdout) {
		if strings.HasPrefix(kvPair, "flow_count=") {
			value := strings.Split(kvPair, "=")[1]
			metricName := bridgeName + "flows_total"
			return parseMetricToFloat(metricName, value)
		}
	}
	klog.Errorf("ovs-ofctl dump-aggregate %s output didn't contain "+
		"flow_count field", bridgeName)
	return 0
}

type interfaceDetails struct {
	bridge string
	port   string
}

// getInterfaceToPortToBrdigeMapping obtains the interface details
// of to which port and bridge it belongs to.
func getInterfaceToPortToBrdigeMapping(portBridgeMap map[string]string) (interfacePortbridgeMap map[string]interfaceDetails,
	err error) {
	var stdout, stderr string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic while parsing the "+
				"ovs-vsctl list Port output :%v", r)
		}
	}()

	stdout, stderr, err = util.RunOVSVsctl("--no-headings", "--data=bare",
		"--format=csv", "--columns=_uuid,name,interfaces", "list", "Port")
	if err != nil {
		return nil, fmt.Errorf("failed to get output for ovs-vsctl list Port "+
			"stderr(%s) :(%v)", stderr, err)
	}
	interfacePortbridgeMap = make(map[string]interfaceDetails)
	// output will be of format:(23967680-7899-44ce-b8d1-dfce6c471624,
	//brenp0s8,3333db76-e2da-4062-a7ee-328d0a380a63)
	for _, kvPair := range strings.Split(stdout, "\n") {
		if kvPair == "" {
			continue
		}
		fields := strings.Split(kvPair, ",")
		portId := fields[0]
		portName := fields[1]
		interfaces := strings.Fields(fields[2])
		for _, interfaceId := range interfaces {
			interfacePortbridgeMap[interfaceId] = interfaceDetails{
				bridge: portBridgeMap[portId],
				port:   portName,
			}
		}
	}
	return interfacePortbridgeMap, nil
}

// getBridgeInfo obtains the (per Brdige port count) &
// port to bridge mapping for each port
func getOvsBridgeInfo() (bridgePortCount map[string]float64, portToBridgeMap map[string]string,
	err error) {
	var stdout, stderr string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from a panic while parsing the "+
				"ovs-vsctl list Bridge output : %v", r)
		}
	}()

	stdout, stderr, err = util.RunOVSVsctl("--no-headings", "--data=bare",
		"--format=csv", "--columns=name,port", "list", "Bridge")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get output for ovs-vsctl list Bridge "+
			"stderr(%s) :(%v)", stderr, err)
	}

	bridgePortCount = make(map[string]float64)
	portToBridgeMap = make(map[string]string)
	//output will be of format :(br-local,12bc8575-8e1f-4583-b693-ea3b5bf09974
	// 5dc87c46-4d94-4469-9f7a-67ee1c8beb03 620cafe4-bfe5-4a23-8165-4ffc61e7de42)
	for _, kvPair := range strings.Split(stdout, "\n") {
		if kvPair == "" {
			continue
		}
		fields := strings.Split(kvPair, ",")
		bridgeName := fields[0]
		ports := strings.Fields(fields[1])
		if bridgeName != "" {
			bridgePortCount[bridgeName] = float64(len(ports))
		}
		for _, portId := range ports {
			portToBridgeMap[portId] = bridgeName
		}
	}
	return bridgePortCount, portToBridgeMap, nil
}

//ovsBridgeMetricsUpdate updates bridgeMetrics & ovsInterface metrics for every 30sec
func ovsBridgeMetricsUpdate() {
	for {
		bridgePortCountMapping, portBridgeMapping, err := getOvsBridgeInfo()
		if err != nil {
			klog.Errorf("%s", err.Error())
			time.Sleep(30 * time.Second)
			continue
		}
		for brName, noPorts := range bridgePortCountMapping {
			metricOvsBridge.WithLabelValues(brName).Set(1)
			metricOvsBridgePortsTotal.WithLabelValues(brName).Set(noPorts)
			flowsCount := getOvsBridgeOpenFlowsCount(brName)
			metricOvsBridgeFlowsTotal.WithLabelValues(brName).Set(flowsCount)
		}
		metricOvsBridgeTotal.Set(float64(len(bridgePortCountMapping)))

		interfaceToPortToBridgeMap, err := getInterfaceToPortToBrdigeMapping(portBridgeMapping)
		if err != nil {
			klog.Errorf("%s", err.Error())
			time.Sleep(30 * time.Second)
			continue
		}
		// set ovs interface metrics.
		err = ovsInterfaceMetricsUpdate(interfaceToPortToBridgeMap)
		if err != nil {
			klog.Errorf("%s", err.Error())
		}
		time.Sleep(30 * time.Second)
	}
}

func registerOvsInterfaceMetrics(metricNamespace, metricSubsystem string) {
	for InterfaceMetricName, InterfaceMetricInfo := range ovsInterfaceMetricsDataMap {
		InterfaceMetricInfo.metric = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      InterfaceMetricName,
			Help:      InterfaceMetricInfo.help,
		},
			[]string{
				"bridge",
				"port",
				"interface",
			})
		prometheus.MustRegister(InterfaceMetricInfo.metric)
	}
}

func getOvsInterfaceType(state string) float64 {
	var typeValue float64
	if state == "" {
		state = "system"
	}
	interfaceTypeMap := map[string]float64{
		"system":   1,
		"internal": 2,
		"tap":      3,
		"geneve":   4,
		"gre":      5,
		"vxlan":    6,
		"lisp":     7,
		"stt":      8,
		"patch":    9,
	}
	if value, ok := interfaceTypeMap[state]; ok {
		typeValue = value
	} else {
		typeValue = 0
	}
	return typeValue
}

func getOvsInterfaceState(state string) float64 {
	var stateValue float64
	if state == "" {
		return 0
	}
	stateMap := map[string]float64{
		"down": 1,
		"up":   2,
	}
	if value, ok := stateMap[state]; ok {
		stateValue = value
	} else {
		stateValue = 0
	}
	return stateValue
}

func setOvsInterfaceMetrics(interfaceBridge, interfacePort, interfaceName, metricName, metricValue string) {
	var value float64
	if metricValue != "" {
		metric := interfaceName + "_" + metricName
		value = parseMetricToFloat(metric, metricValue)
	} else {
		value = 0
	}
	ovsInterfaceMetricsDataMap[metricName].metric.WithLabelValues(interfaceBridge,
		interfacePort, interfaceName).Set(value)
}

func setOvsInterfaceStatistics(interfaceBridge, interfacePort, interfaceName, metricValue string) {
	var InterfaceStats = []string{
		"rx_packets",
		"rx_bytes",
		"rx_dropped",
		"rx_frame_err",
		"rx_over_err",
		"rx_crc_err",
		"rx_errors",
		"tx_packets",
		"tx_bytes",
		"tx_dropped",
		"collisions",
		"tx_errors",
	}
	//metricValue will be of format:(rx_bytes=20566 rx_packets=213 tx_bytes=2940 tx_packets=70)
	statsMap := make(map[string]float64)
	for _, field := range strings.Fields(metricValue) {
		statsField := strings.Split(field, "=")
		metric := interfaceName + "_" + statsField[0]
		statName := strings.TrimSpace(statsField[0])
		statValue := strings.TrimSpace(statsField[1])
		statsMap[statName] = parseMetricToFloat(metric, statValue)
	}
	var statValue float64
	for _, stat := range InterfaceStats {
		metricName := "interface_" + stat
		if value, ok := statsMap[stat]; ok {
			statValue = value
		} else {
			statValue = 0
		}
		ovsInterfaceMetricsDataMap[metricName].metric.WithLabelValues(interfaceBridge,
			interfacePort, interfaceName).Set(statValue)
	}
}

// ovsInterfaceMetricsUpdate updates the ovs interface metrics
// obained from ovs-vsctl --columns=<fields> list interface
func ovsInterfaceMetricsUpdate(interfaceInfo map[string]interfaceDetails) (err error) {
	interfaceColumnFields := []string{
		"_uuid",
		"name",
		"duplex",
		"type",
		"admin_state",
		"link_state",
		"statistics",
		"ifindex",
		"link_resets",
		"link_speed",
		"mtu",
		"ofport",
		"ingress_policing_burst",
		"ingress_policing_rate",
	}
	var stdout, stderr string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from panic while parsing the ovs-vsctl "+
				"list Interface output : %v", r)
		}
	}()

	interfaceFieldsList := strings.Join(interfaceColumnFields, ",")
	stdout, stderr, err = util.RunOVSVsctl("--no-headings", "--data=bare",
		"--format=csv", "--columns="+interfaceFieldsList, "list", "Interface")
	if err != nil {
		return fmt.Errorf("failed to get output for ovs-vsctl list Interface "+
			"stderr(%s) :(%v)", stderr, err)
	}

	for _, kvPair := range strings.Split(stdout, "\n") {
		if kvPair == "" {
			continue
		}
		interfaceFieldValues := strings.Split(kvPair, ",")
		interfaceId := interfaceFieldValues[0]
		interfaceName := interfaceFieldValues[1]
		interfaceData := interfaceInfo[interfaceId]

		var duplexValue float64
		if interfaceFieldValues[2] == "half" {
			duplexValue = 0
		} else if interfaceFieldValues[2] == "full" {
			duplexValue = 1
		} else {
			duplexValue = 2
		}
		ovsInterfaceMetricsDataMap["interface_duplex"].metric.WithLabelValues(
			interfaceData.bridge, interfaceData.port, interfaceName).Set(duplexValue)
		interfaceTypeValue := getOvsInterfaceType(interfaceFieldValues[3])
		ovsInterfaceMetricsDataMap["interface_type"].metric.WithLabelValues(
			interfaceData.bridge, interfaceData.port, interfaceName).Set(interfaceTypeValue)
		adminStateValue := getOvsInterfaceState(interfaceFieldValues[4])
		ovsInterfaceMetricsDataMap["interface_admin_state"].metric.WithLabelValues(
			interfaceData.bridge, interfaceData.port, interfaceName).Set(adminStateValue)
		linkStatevalue := getOvsInterfaceState(interfaceFieldValues[5])
		ovsInterfaceMetricsDataMap["interface_link_state"].metric.WithLabelValues(
			interfaceData.bridge, interfaceData.port, interfaceName).Set(linkStatevalue)
		setOvsInterfaceStatistics(interfaceData.bridge, interfaceData.port,
			interfaceName, interfaceFieldValues[6])
		setOvsInterfaceMetrics(interfaceData.bridge, interfaceData.port, interfaceName,
			"interface_ifindex", interfaceFieldValues[7])
		setOvsInterfaceMetrics(interfaceData.bridge, interfaceData.port, interfaceName,
			"interface_link_resets", interfaceFieldValues[8])
		setOvsInterfaceMetrics(interfaceData.bridge, interfaceData.port, interfaceName,
			"interface_link_speed", interfaceFieldValues[9])
		setOvsInterfaceMetrics(interfaceData.bridge, interfaceData.port, interfaceName,
			"interface_mtu", interfaceFieldValues[10])
		setOvsInterfaceMetrics(interfaceData.bridge, interfaceData.port, interfaceName,
			"interface_of_port", interfaceFieldValues[11])
		setOvsInterfaceMetrics(interfaceData.bridge, interfaceData.port, interfaceName,
			"interface_ingress_policing_burst", interfaceFieldValues[12])
		setOvsInterfaceMetrics(interfaceData.bridge, interfaceData.port, interfaceName,
			"interface_ingress_policing_rate", interfaceFieldValues[13])
	}
	return nil
}

// setOvsMemoryMetrics updates the handlers, revalidators
// count from "ovs-appctl -t ovs-vswitchd memory/show" output.
func setOvsMemoryMetrics() (err error) {
	var stdout, stderr string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from panic while parsing the ovs-appctl "+
				"memory/show output : %v", r)
		}
	}()

	stdout, stderr, err = util.RunOvsVswitchdAppCtl("memory/show")
	if err != nil {
		return fmt.Errorf("failed to retrieve memory/show output "+
			"for ovs-vswitchd stderr(%s) :%v", stderr, err)
	}

	for _, kvPair := range strings.Fields(stdout) {
		if strings.HasPrefix(kvPair, "handlers:") {
			value := strings.Split(kvPair, ":")[1]
			count := parseMetricToFloat("handlers_total", value)
			metricOvsHandlersTotal.Set(count)
		} else if strings.HasPrefix(kvPair, "revalidators:") {
			value := strings.Split(kvPair, ":")[1]
			count := parseMetricToFloat("revalidators_total", value)
			metricOvsRevalidatorsTotal.Set(count)
		}
	}
	return nil
}

func ovsMemoryMetricsUpdate() {
	for {
		err := setOvsMemoryMetrics()
		if err != nil {
			klog.Errorf("%s", err.Error())
		}
		time.Sleep(30 * time.Second)
	}
}

// setOvsHwOffloadMetrics obatains the hw-offlaod, tc-policy
// ovs-vsctl list Open_vSwitch . and updates the corresponding metrics
func setOvsHwOffloadMetrics() (err error) {
	var stdout, stderr string

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovering from panic while parsing the ovs-vsctl "+
				"list Open_vSwitch . output : %v", r)
		}
	}()

	stdout, stderr, err = util.RunOVSVsctl("--no-headings", "--data=bare",
		"--columns=other_config", "list", "Open_vSwitch", ".")
	if err != nil {
		return fmt.Errorf("failed to get output from ovs-vsctl list --columns=other_config"+
			"open_vSwitch . stderr(%s) : %v", stderr, err)
	}

	var hwOffloadValue = "false"
	var tcPolicyValue = "none"
	var tcPolicyMap = map[string]float64{
		"none":    0,
		"skip_sw": 1,
		"skip_hw": 2,
	}
	for _, kvPair := range strings.Fields(stdout) {
		if strings.HasPrefix(kvPair, "hw-offload=") {
			hwOffloadValue = strings.Split(kvPair, "=")[1]
		} else if strings.HasPrefix(kvPair, "tc-policy=") {
			tcPolicyValue = strings.Split(kvPair, "=")[1]
		}
	}

	if hwOffloadValue == "false" {
		metricOvsHwOffload.Set(0)
	} else {
		metricOvsHwOffload.Set(1)
	}
	metricOvsTcPolicy.Set(tcPolicyMap[tcPolicyValue])
	return nil
}

func ovsHwOffloadMetricsUpdate() {
	for {
		err := setOvsHwOffloadMetrics()
		if err != nil {
			klog.Errorf("%s", err.Error())
		}
		time.Sleep(30 * time.Second)
	}
}

type ovsInterfaceMetricsDetails struct {
	help   string
	metric *prometheus.GaugeVec
}

var ovsInterfaceMetricsDataMap = map[string]*ovsInterfaceMetricsDetails{
	"interface_rx_packets": {
		help: "Represents the number of received packets " +
			"by OVS interface.",
	},
	"interface_rx_bytes": {
		help: "Represents the number of received bytes by " +
			"OVS interface.",
	},
	"interface_rx_dropped": {
		help: "Represents the number of input packets dropped " +
			"by OVS interface.",
	},
	"interface_rx_frame_err": {
		help: "Represents the number of frame alignment errors " +
			"on the packets received by OVS interface.",
	},
	"interface_rx_over_err": {
		help: "Represents the number of packets with RX overrun " +
			"received by OVS interface.",
	},
	"interface_rx_crc_err": {
		help: "Represents the number of CRC errors for the packets " +
			"received by OVS interface.",
	},
	"interface_rx_errors": {
		help: "Represents the total number of packets with errors " +
			"received by OVS interface.",
	},
	"interface_tx_packets": {
		help: "Represents the number of transmitted packets by " +
			"OVS interface.",
	},
	"interface_tx_bytes": {
		help: "Represents the number of transmitted bytes " +
			"by OVS interface.",
	},
	"interface_tx_dropped": {
		help: "Represents the number of output packets dropped " +
			"by OVS interface.",
	},
	"interface_collisions": {
		help: "Represents the number of collisions " +
			"on the packets transmitted by OVS interface.",
	},
	"interface_tx_errors": {
		help: "Represents the total number of packets with errors " +
			"transmitted by OVS interface.",
	},
	"interface_ingress_policing_rate": {
		help: "Maximum rate for data received on OVS interface, " +
			"in kbps. If the value is 0, then policing is disabled.",
	},
	"interface_ingress_policing_burst": {
		help: "Maximum burst size for data received on OVS interface, " +
			"in kb. The default burst size if set to 0 is 8000 kbit.",
	},
	"interface_admin_state": {
		help: "The administrative state of the OVS interface. " +
			"The values are: other(0), down(1) or up(2).",
	},
	"interface_link_state": {
		help: "The link state of the OVS interface. " +
			"The values are: down(1) or up(2) or other(0).",
	},
	"interface_type": {
		help: "Represents the interface type other(0), system(1), internal(2), " +
			"tap(3), geneve(4), gre(5), vxlan(6), lisp(7), stt(8), patch(9).",
	},
	"interface_mtu": {
		help: "The currently configured MTU for OVS interface.",
	},
	"interface_of_port": {
		help: "Represents the OpenFlow port ID associated with OVS interface.",
	},
	"interface_duplex": {
		help: "The duplex mode of the OVS interface. The values are half(0) " +
			"or full(1) or other(2)",
	},
	"interface_ifindex": {
		help: "Represents the interface index associated with OVS interface.",
	},
	"interface_link_speed": {
		help: "The negotiated speed of the OVS interface.",
	},
	"interface_link_resets": {
		help: "The number of times Open vSwitch has observed the " +
			"link_state of OVS interface change.",
	},
}

var ovsVswitchdCoverageShowCountersMap = map[string]*metricDetails{
	"netlink_sent": {
		help: "Number of netlink message sent to the kernel.",
	},
	"netlink_received": {
		help: "Number of netlink messages received by the kernel.",
	},
	"netlink_recv_jumbo": {
		help: "Number of netlink messages that were received from" +
			"the kernel were more than the allocated buffer.",
	},
	"netlink_overflow": {
		help: "Netlink messages dropped by the daemon due " +
			"to buffer overflow.",
	},
	"rconn_sent": {
		help: "Specifies the number of messages " +
			"that have been sent to the underlying virtual " +
			"connection (unix, tcp, or ssl) to OpenFlow devices.",
	},
	"rconn_queued": {
		help: "Specifies the number of messages that have been " +
			"queued because it couldn’t be sent using the " +
			"underlying virtual connection to OpenFlow devices.",
	},
	"rconn_discarded": {
		help: "Specifies the number of messages that " +
			"have been dropped because the send queue " +
			"had to be flushed because of reconnection.",
	},
	"rconn_overflow": {
		help: "Specifies the number of messages that have " +
			"been dropped because of the queue overflow.",
	},
	"vconn_open": {
		help: "Specifies the number of attempts to connect " +
			"to an OpenFlow Device.",
	},
	"vconn_sent": {
		help: "Specifies the number of messages sent " +
			"to the OpenFlow Device.",
	},
	"vconn_received": {
		help: "Specifies the number of messages received " +
			"from the OpenFlow Device.",
	},
	"pstream_open": {
		help: "Specifies the number of time passive connections " +
			"were opened for the remote peer to connect.",
	},
	"stream_open": {
		help: "Specifies the number of attempts to connect " +
			"to a remote peer (active connection).",
	},
	"txn_success": {
		help: "Specifies the number of times the OVSDB " +
			"transaction has successfully completed.",
	},
	"txn_error": {
		help: "Specifies the number of times the OVSDB " +
			"transaction has errored out.",
	},
	"txn_uncommitted": {
		help: "Specifies the number of times the OVSDB " +
			"transaction were uncommitted.",
	},
	"txn_unchanged": {
		help: "Specifies the number of times the OVSDB transaction " +
			"resulted in no change to the database.",
	},
	"txn_incomplete": {
		help: "Specifies the number of times the OVSDB transaction " +
			"did not complete and the client had to re-try.",
	},
	"txn_aborted": {
		help: "Specifies the number of times the OVSDB " +
			" transaction has been aborted.",
	},
	"txn_try_again": {
		help: "Specifies the number of times the OVSDB " +
			"transaction failed and the client had to re-try.",
	},
	"dpif_port_add": {
		help: "Number of times a netdev was added as a port to the dpif.",
	},
	"dpif_port_del": {
		help: "Number of times a netdev was removed from the dpif.",
	},
	"dpif_flow_flush": {
		help: "Number of times flows were flushed from the datapath " +
			"(Linux kernel datapath module).",
	},
	"dpif_flow_get": {
		help: "Number of times flows were retrieved from the " +
			"datapath (Linux kernel datapath module).",
	},
	"dpif_flow_put": {
		help: "Number of times flows were added to the datapath " +
			"(Linux kernel datapath module).",
	},
	"dpif_flow_del": {
		help: "Number of times flows were deleted from the " +
			"datapath (Linux kernel datapath module).",
	},
	"bridge_reconfigure": {
		help: "Number of times OVS bridges were reconfigured.",
	},
	"xlate_actions": {
		help: "Number of times an OpenFlow actions were translated " +
			"into datapath actions.",
	},
	"xlate_actions_oversize": {
		help: "Number of times the translated OpenFlow actions into " +
			"a datapath actions were too big for a netlink attribute.",
	},
	"xlate_actions_too_many_output": {
		help: "Number of times the number of datapath actions " +
			"were more than what the kernel can handle reliably.",
	},
	"packet_in": {
		help: "Specifies the number of times ovs-vswitchd has " +
			"handled the packet-ins on behalf of kernel datapath.",
	},
}
var registerOvsMetricsOnce sync.Once

func RegisterOvsMetrics() {
	registerOvsMetricsOnce.Do(func() {
		getOvsVersionInfo()
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: MetricOvsNamespace,
				Name:      "build_info",
				Help:      "A metric with a constant '1' value labeled by ovs version.",
				ConstLabels: prometheus.Labels{
					"version": ovsVersion,
				},
			},
			func() float64 { return 1 },
		))
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Namespace: MetricOvsNamespace,
				Subsystem: MetricOvsSubsystemVswitchd,
				Name:      "dpif_execute",
				Help: "Number of times the OpenFlow actions were executed in userspace " +
					"on behalf of the datapath.",
			}, func() float64 {
				counters, err := coverageShowCounters(ovsVswitchd)
				if err != nil {
					klog.Errorf("%s", err.Error())
					return 0
				}

				dpIfExecuteCounters := []string{
					"dpif_execute",
					"dpif_execute_with_help",
				}
				var dpIfExecuteMetricValue float64
				for _, counterName := range dpIfExecuteCounters {
					if value, ok := counters[counterName]; ok {
						dpIfExecuteMetricValue += parseMetricToFloat(counterName, value)
					}
				}
				return dpIfExecuteMetricValue
			}))

		// Register OVS datapath metrics.
		prometheus.MustRegister(metricOvsDpTotal)
		prometheus.MustRegister(metricOvsDp)
		prometheus.MustRegister(metricOvsDpIfTotal)
		prometheus.MustRegister(metricOvsDpIf)
		prometheus.MustRegister(metricOvsDpFlowsTotal)
		prometheus.MustRegister(metricOvsDpFlowsLookupHit)
		prometheus.MustRegister(metricOvsDpFlowsLookupMissed)
		prometheus.MustRegister(metricOvsDpFlowsLookupLost)
		prometheus.MustRegister(metricOvsDpPacketsTotal)
		prometheus.MustRegister(metricOvsdpMasksHit)
		prometheus.MustRegister(metricOvsDpMasksTotal)
		prometheus.MustRegister(metricOvsDpMasksHitRatio)
		// Register OVS bridge statistics & attributes metrics
		prometheus.MustRegister(metricOvsBridgeTotal)
		prometheus.MustRegister(metricOvsBridge)
		prometheus.MustRegister(metricOvsBridgePortsTotal)
		prometheus.MustRegister(metricOvsBridgeFlowsTotal)
		// Register Ovsvswitchd Memory metrics
		prometheus.MustRegister(metricOvsHandlersTotal)
		prometheus.MustRegister(metricOvsRevalidatorsTotal)
		// Register Ovs hwoffload metrics
		prometheus.MustRegister(metricOvsHwOffload)
		prometheus.MustRegister(metricOvsTcPolicy)
		// Register ovs Interface metrics with prometheus.
		registerOvsInterfaceMetrics(MetricOvsNamespace, MetricOvsSubsystemVswitchd)
		// Register the ovs-vswitchd coverage/show counters metrics with prometheus
		registerCoverageShowCounters(ovsVswitchd, MetricOvsNamespace, MetricOvsSubsystemVswitchd)

		// ovs datapath metrics updater
		go ovsDataPathMetricsUpdate()
		// ovs bridge metrics updater
		go ovsBridgeMetricsUpdate()
		// ovs memory metrics updater
		go ovsMemoryMetricsUpdate()
		// ovs HWOffload metrics updater
		go ovsHwOffloadMetricsUpdate()
		// ovs coverage show counters metrics updater.
		go coverageShowCountersMetricsUpdater(ovsVswitchd)
	})
}
