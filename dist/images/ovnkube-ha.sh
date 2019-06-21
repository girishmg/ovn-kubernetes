#!/bin/bash
#set -x
#set -euo pipefail

# This script is the entrypoint to the ovnkube-db-ha image.
# Supports version 3 daemonsets only
# Commands ($1 values)
#    run-ovn-db-cluster Runs NB/SB OVSDB in pacemaker/corosync cluster (v3)
#
# NOTE: The script/image must be compatible with the daemonset.
# This script supports version 3 daemonsets
#    When called, it starts the needed daemons to provide pacemaker/corosync OVN DB.

# ====================

. "./ovnkube-lib" || exit 1

cmd=${1}

# =========================================

# Check the ovndb_server pcs resource status. return 2 if
# the status is unknown, return 1 if the status is
# stopped, return 0 if status is started (master or slave)
crm_node_status () {
  retcode=2
  crm stat  | egrep 'Masters|Slaves|Stopped' | awk '$1=$1' > /tmp/crm_stat.$$
  if [[ $? -ne 0 ]] ; then
    echo Command "crm stat" failed
    return $retcode
  fi

  IFS=' ' read -a master_nodes <<< "$(awk -F'[][]' '{if ($1 ~ /Slaves/) print $2;}' /tmp/crm_stat.$$)"
  IFS=' ' read -a slave_nodes <<< "$(awk -F'[][]' '{if ($1 ~ /Masters/) print $2;}' /tmp/crm_stat.$$)"
  IFS=' ' read -a stopped_nodes <<< "$(awk -F'[][]' '{if ($1 ~ /Stopped/) print $2;}' /tmp/crm_stat.$$)"
  started_nodes=("${master_nodes[@]}" "${slave_nodes[@]}")
  if [[ " ${master_nodes[@]} " =~ " ${ovn_pod_host} " ]]; then
    retcode=0
  elif [[ " ${slave_nodes[@]} " =~ " ${ovn_pod_host} " ]]; then
    retcode=0
  elif [[ " ${stopped_nodes[@]} " =~ " ${ovn_pod_host} " ]]; then
    retcode=1
  fi
  rm -rf /tmp/crm_stat.$$
  echo "cluster status $retcode"
  return $retcode
}

config_corosync () {
  # get all the nodes that are participating in hosting the OVN DBs
  cluster_nodes=$(kubectl --server=${K8S_APISERVER} --token=${k8s_token} --certificate-authority=${K8S_CACERT} \
    get nodes --selector=openvswitch.org/ovnkube-db=true \
    -o=jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null)
  nnodes=$(echo $cluster_nodes |wc -w)
  if [[ $nnodes -lt 2 ]]; then
    echo "at least 2 nodes need to be configured"
    exit 1
  fi
  echo cluster_nodes=$cluster_nodes

  cat << EOF > /etc/corosync/corosync.conf
totem {
  version: 2
  cluster_name: ovn-central-cluster
  transport: udpu
  secauth: off
}
quorum {
  provider: corosync_votequorum
}
logging {
  to_logfile: yes
  logfile: /var/log/corosync/corosync.log
  to_syslog: yes
  timestamp: on
}
nodelist {
`for h in $cluster_nodes; do printf "  node {\n    ring0_addr: $h\n  }\n"; done`
}
EOF
}

service_start() {
  for svc in "$@"; do
    service ${svc} start >> /dev/null 2>&1
  done
}

service_healthy() {
  for svc in "$@"; do
    service ${svc} status | grep "is running" >> /dev/null 2>&1
    if [[ $? -ne 0 ]]; then
      echo "service ${svc} is not started"
      return 1
    fi
  done
  return 0
}

# v3 - Runs ovn NB/SB DB pacemaker/corosync cluster
run-ovn-db-cluster () {
  check_ovn_daemonset_version "3"
  if [[ ${ovn_db_ha_vip} == "" ]] ; then
    echo "Exiting since the Virtual IP to be used for OVN DBs has not been provided"
    return 1
  fi
  trap stop-ovn-db-cluster TERM

  config_corosync

  service_start pcsd corosync pacemaker
  service_healthy pcsd corosync pacemaker
  if [[ $? -ne 0 ]]; then
    exit 11
  fi

  pcs property set stonith-enabled=false > /dev/null 2>&1
  pcs property set no-quorum-policy=ignore > /dev/null 2>&1
  pcs resource create VirtualIP ocf:heartbeat:IPaddr2 ip=${ovn_db_ha_vip} op monitor interval=30s > /dev/null 2>&1
  pcs resource create ovndb_servers ocf:ovn:ovndb-servers master_ip=${ovn_db_ha_vip}  \
    inactive_probe_interval=0 ovn_ctl=/usr/share/openvswitch/scripts/ovn-ctl \
    op monitor interval="60s" timeout="50s" op monitor role=Master interval="15s" > /dev/null 2>&1
  pcs resource master ovndb_servers-master ovndb_servers meta notify="true" > /dev/null 2>&1
  pcs constraint order promote ovndb_servers-master then VirtualIP > /dev/null 2>&1
  pcs constraint colocation add VirtualIP with master ovndb_servers-master score=INFINITY > /dev/null 2>&1

  retries=0
  started=0
  while true; do
    # Check resource status on this node
    crm_node_status
    ret=$?

    if [[ $started -eq 0 ]]; then
      if [[ $ret -eq 0 ]]; then
        echo "OVN DB HA cluster started after ${retries} retries"
        started=1

        # now we can create the ovnkube-db endpoint with the ${ovn_db_ha_vip} as the IP address. with that
        # the waiting ovnkube-node and ovnkube-master PODs will continue
        create_ovnkube_db_ep
      elif [[ $ret -eq 1 ]]; then
	echo pcs node unstandby ${ovn_pod_host}
        pcs node unstandby ${ovn_pod_host}
      elif [[ "${retries}" -gt 60 ]]; then
        echo "after 60 retries, Corosync/Packemaker OVN DB cluster didn't start"
        exit 11
      fi
    elif [[ $ret -ne 0 ]]; then
      # Service stopped for some reason, reset the retry and restart
      echo "Unknown OVN DB HA cluster status. Attempting to restart the cluster"
      started=0
      retries=0
    fi
    sleep 1
    (( retries += 1 ))
  done
}

# v3 - standby ovn NB/SB DB cluster on this node
# this will gracefully stop the cluster on this
# node and unplumb the VIP if needed
stop-ovn-db-cluster () {
  echo pcs node standby ${ovn_pod_host}
  pcs node standby ${ovn_pod_host}
  retries=0
  while true; do
    crm_node_status
    ret=$?
    if [[ $ret -ne 0 ]] ; then
      break
    fi
    (( retries += 1 ))
    if [[ "${retries}" -gt 30 ]]; then
      echo "error: OVN DB cluster failed to be stopped gracefully"
      break
    fi
    sleep 1
  done
}


echo "================== ovnkube-ha.sh --- version: ${ovnkube_version} ================"

  echo " ==================== command: ${cmd}"

  # display_env

# Start the requested daemons
# daemons come up in order
# ovs-db-server  - all nodes  -- not done by this script (v2 v3)
# ovs-vswitchd   - all nodes  -- not done by this script (v2 v3)
#  run-ovn-northd Runs ovn-northd as a process does not run nb_ovsdb or sb_ovsdb (v3)
#  nb-ovsdb       Runs nb_ovsdb as a process (no detach or monitor) (v3)
#  sb-ovsdb       Runs sb_ovsdb as a process (no detach or monitor) (v3)
# ovn-northd     - master node only (v2)
# ovn-master     - master node only (v2 v3)
# ovn-controller - all nodes (v2 v3)
# ovn-node       - all nodes (v2 v3)
# run-ovn-db-cluster - Runs NB/SB OVSDB in pacemaker/corosync cluster (v3)

  case ${cmd} in
    "run-ovn-db-cluster")
    run-ovn-db-cluster
    ;;
    *)
	echo "invalid command ${cmd}"
	echo "valid v3 commands: run-ovn-db-cluster"
	exit 0
  esac

exit 0
