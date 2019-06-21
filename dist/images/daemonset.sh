#!/bin/bash
#set -x

#Always exit on errors
set -e

# This is people that are nut using the ansible install.
# The script expands the templates into yaml files in ../yaml

OVN_IMAGE=""
OVN_HA_IMAGE=""
OVN_IMAGE_PULL_POLICY=""
OVN_NET_CIDR=""
OVN_SVC_DIDR=""
OVN_K8S_APISERVER=""
OVN_GATEWAY_OPTS=""
OVN_DB_REPLICAS=""
OVN_DB_HA_VIP=""

# Parse parameters given as arguments to this script.
while [ "$1" != "" ]; do
    PARAM=`echo $1 | awk -F= '{print $1}'`
    VALUE=`echo $1 | awk -F= '{print $2}'`
    case $PARAM in
        --image)
            OVN_IMAGE=$VALUE
            ;;
        --ha-image)
            OVN_HA_IMAGE=$VALUE
            ;;
        --image-pull-policy)
            OVN_IMAGE_PULL_POLICY=$VALUE
            ;;
        --gateway-options)
            OVN_GATEWAY_OPTS=$VALUE
            ;;
        --net-cidr)
            OVN_NET_CIDR=$VALUE
            ;;
        --svc-cidr)
            OVN_SVC_CIDR=$VALUE
            ;;
        --k8s-apiserver)
            OVN_K8S_APISERVER=$VALUE
            ;;
        --db-replicas)
            OVN_DB_REPLICAS=$VALUE
            ;;
        --db-ha-vip)
            OVN_DB_HA_VIP=$VALUE
            ;;
        *)
            echo "WARNING: unknown parameter \"$PARAM\""
            exit 1
            ;;
    esac
    shift
done

# The options provided on the CLI overrides the values in ../ansible/hosts

# Create the daemonsets with the desired image
# The image name is from ../ansible/hosts
# The daemonset.yaml files are templates in ../ansible/templates
# They are expanded into daemonsets in ../yaml

if [[ ${OVN_IMAGE} == "" ]]
then
    image=$(awk -F = '/^ovn_image=/{ print $2 }' ../ansible/hosts | sed 's/\"//g')
    if [[ ${image} == "" ]]
    then
      image="docker.io/ovnkube/ovn-daemonset:latest"
    fi
else
    image=$OVN_IMAGE
fi
echo "image: ${image}"

ha_image=""
if [[ ${OVN_HA_IMAGE} == "" ]]
then
    ha_image=$(awk -F = '/^ovn_ha_image=/{ print $2 }' ../ansible/hosts | sed 's/\"//g')
else
    ha_image=$OVN_HA_IMAGE
fi
echo "ha_image: ${ha_image}"

if [[ ${OVN_IMAGE_PULL_POLICY} == "" ]]
then
    policy=$(awk -F = '/^ovn_image_pull_policy/{ print $2 }' ../ansible/hosts)
    if [[ ${policy} == "" ]]
    then
      policy="IfNotPresent"
    fi
else
    policy=$OVN_IMAGE_PULL_POLICY
fi
echo "imagePullPolicy: ${policy}"

ovn_gateway_opts=${OVN_GATEWAY_OPTS}
echo "ovn_gateway_opts: ${ovn_gateway_opts}"

ovn_db_replicas=${OVN_DB_REPLICAS:-3}
echo "ovn_db_replicas: ${ovn_db_replicas}"
ovn_db_ha_vip=${OVN_DB_HA_VIP}
echo "ovn_db_ha_vip: ${ovn_db_ha_vip}"

# Simplified expansion of template 
image_str="{{ ovn_image | default('docker.io/ovnkube/ovn-daemonset:latest') }}"
ha_image_str="{{ ovn_ha_image | default('docker.io/ovnkube/ovn-ha-daemonset:latest') }}"
policy_str="{{ ovn_image_pull_policy | default('IfNotPresent') }}"
ovn_gateway_opts_repl="{{ ovn_gateway_opts }}"
ovn_db_replicas_repl="{{ ovn_db_replicas | default(3) }}"
ovn_db_ha_vip_repl="{{ ovn_db_ha_vip }}"

sed "s,${image_str},${image},
s,${ovn_gateway_opts_repl},${ovn_gateway_opts},
s,${policy_str},${policy}," ../templates/ovnkube-node.yaml.j2 > ../yaml/ovnkube-node.yaml

sed "s,${image_str},${image},
s,${policy_str},${policy}," ../templates/ovnkube-master.yaml.j2 > ../yaml/ovnkube-master.yaml

sed "s,${image_str},${image},
s,${policy_str},${policy}," ../templates/ovnkube-db.yaml.j2 > ../yaml/ovnkube-db.yaml

# only generate ovnkube-db-ha yaml if OVN_HA_IMAGE is specified
if [[ ${ha_image} != "" ]]
then
    sed "s,${ha_image_str},${ha_image},
    s,${ovn_db_replicas_repl},${ovn_db_replicas},
    s,${ovn_db_ha_vip_repl},${ovn_db_ha_vip},
    s,${policy_str},${policy}," ../templates/ovnkube-db-ha.yaml.j2 > ../yaml/ovnkube-db-ha.yaml
fi

# ovn-setup.yaml
# net_cidr=10.128.0.0/14/23
# svc_cidr=172.30.0.0/16

if [[ ${OVN_NET_CIDR} == "" ]]
then
    net_cidr=$(awk -F = '/^net_cidr=/{ print $2 }' ../ansible/hosts)
    if [[ ${net_cidr} == "" ]]
    then
      net_cidr="10.128.0.0/14/23"
    fi
else
    net_cidr=$OVN_NET_CIDR
fi

if [[ ${OVN_SVC_CIDR} == "" ]]
then
    svc_cidr=$(awk -F = '/^svc_cidr=/{ print $2 }' ../ansible/hosts)
    if [[ ${svc_cidr} == "" ]]
    then
      svc_cidr="172.30.0.0/16"
    fi
else
    svc_cidr=$OVN_SVC_CIDR
fi

k8s_apiserver=${OVN_K8S_APISERVER:-10.0.2.16:6443}

net_cidr_repl="{{ net_cidr | default('10.128.0.0/14/23') }}"
svc_cidr_repl="{{ svc_cidr | default('172.30.0.0/16') }}"
k8s_apiserver_repl="{{ k8s_apiserver.stdout }}"

echo "net_cidr: ${net_cidr}"
echo "svc_cidr: ${svc_cidr}"
echo "k8s_apiserver: ${k8s_apiserver}"

sed "s,${net_cidr_repl},${net_cidr},
s,${svc_cidr_repl},${svc_cidr},
s,${k8s_apiserver_repl},${k8s_apiserver}," ../templates/ovn-setup.yaml.j2 > ../yaml/ovn-setup.yaml

exit 0
