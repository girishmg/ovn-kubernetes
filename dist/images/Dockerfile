#
# This is the OpenShift ovn overlay network image.
# it provides an overlay network using ovs/ovn/ovn-kube
#
# The standard name for this image is ovn-kube

# Notes:
# This is for a development build where the ovn-kubernetes utilities
# are built in this Dockerfile and included in the image (instead of the rpm)
#
# This is based on centos:7
# openvswitch rpms are from
# http://cbs.centos.org/kojifiles/packages/openvswitch/2.9.0/4.el7/x86_64/
#
# So this file will change over time.

FROM centos:8

USER root

ENV PYTHONDONTWRITEBYTECODE yes

COPY kubernetes.repo /etc/yum.repos.d/kubernetes.repo
RUN INSTALL_PKGS=" \
	bind-utils procps-ng openssl numactl-libs firewalld-filesystem \
	libpcap kubectl \
	iproute strace socat \
	unbound-libs python2 \
        " && \
	yum install -y --setopt=tsflags=nodocs --setopt=skip_missing_names_on_install=False $INSTALL_PKGS

RUN yum -y update && yum clean all && rm -rf /var/cache/yum/*

# Get a reasonable version of openvswitch (2.9.2 or higher)
# docker build --build-arg rpmArch=ARCH -f Dockerfile.centos -t some_tag .
# where ARCH can be x86_64 (default), aarch64, or ppc64le
ARG rpmArch=x86_64
ARG dpdkVer=17.11
ARG dpdkSubVer=3.el7
ARG dpdkRpmUrl=http://cbs.centos.org/kojifiles/packages/dpdk/${dpdkVer}/${dpdkSubVer}/${rpmArch}/dpdk-${dpdkVer}-${dpdkSubVer}.${rpmArch}.rpm

RUN if [ "$(uname -m)" = "aarch64" ]; then dpdkRpmUrl=https://rpmfind.net/linux/fedora/linux/updates/28/Everything/aarch64/Packages/d/dpdk-17.11.2-1.fc28.aarch64.rpm; fi && rpm -i ${dpdkRpmUrl}

RUN curl -s https://6bae5606566b26d357c13137b70f0398fee21dee42e9ecdf:@packages.nvidia.com/install/repositories/sdn/nv-ovs-2-13-cicd/script.rpm.sh | bash
RUN curl -s https://47bc9e978f9d6e3f9fd04e6d69613517f50f8a1b78bdbd31:@packages.nvidia.com/install/repositories/sdn/nv-ovn-20-03-cicd/script.rpm.sh | bash

RUN yum install -y epel-release
RUN yum install -y openvswitch ovn ovn-host ovn-central openvswitch-devel

RUN rm -rf /var/cache/yum

RUN mkdir -p /var/run/openvswitch

# Built in ../../go_controller, then the binaries are copied here.
# put things where they are in the rpm
RUN mkdir -p /usr/libexec/cni/
COPY ovnkube ovn-kube-util /usr/bin/
COPY ovn-k8s-cni-overlay /usr/libexec/cni/ovn-k8s-cni-overlay

# ovnkube.sh is the entry point. This script examines environment
# variables to direct operation and configure ovn
COPY ovnkube.sh /root/
COPY ovndb-raft-functions /root/
COPY chassis-del.sh /root/
# override the rpm's ovn_k8s.conf with this local copy
COPY ovn_k8s.conf /etc/openvswitch/ovn_k8s.conf

# copy git commit number into image
COPY git_info /root

# iptables wrappers
COPY ./iptables-scripts/iptables /usr/sbin/
COPY ./iptables-scripts/iptables-save /usr/sbin/
COPY ./iptables-scripts/iptables-restore /usr/sbin/
COPY ./iptables-scripts/ip6tables /usr/sbin/
COPY ./iptables-scripts/ip6tables-save /usr/sbin/
COPY ./iptables-scripts/ip6tables-restore /usr/sbin/

LABEL io.k8s.display-name="ovn kubernetes" \
      io.k8s.description="This is a component of OpenShift Container Platform that provides an overlay network using ovn." \
      io.openshift.tags="openshift" \
      maintainer="Phil Cameron <pcameron@redhat.com>"

WORKDIR /root
ENTRYPOINT /root/ovnkube.sh
