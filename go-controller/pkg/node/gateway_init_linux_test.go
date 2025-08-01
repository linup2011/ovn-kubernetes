//go:build linux
// +build linux

package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/k8snetworkplumbingwg/sriovnet"
	"github.com/stretchr/testify/mock"
	"github.com/urfave/cli/v2"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	udnfakeclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/managementport"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	nodemocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node"
	linkMock "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilMock "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	multinetworkmocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The base expected nftables rules. You must substitute in the management port interface name.
const baseNFTRulesFmt = `
add table inet ovn-kubernetes
add set inet ovn-kubernetes mgmtport-no-snat-nodeports { type inet_proto . inet_service ; comment "NodePorts not subject to management port SNAT" ; }
add set inet ovn-kubernetes mgmtport-no-snat-services-v4 { type ipv4_addr . inet_proto . inet_service ; comment "eTP:Local short-circuit not subject to management port SNAT (IPv4)" ; }
add set inet ovn-kubernetes mgmtport-no-snat-services-v6 { type ipv6_addr . inet_proto . inet_service ; comment "eTP:Local short-circuit not subject to management port SNAT (IPv6)" ; }
add set inet ovn-kubernetes mgmtport-no-snat-subnets-v4 { type ipv4_addr ; flags interval ; comment "subnets not subject to management port SNAT (IPv4)" ; }
add set inet ovn-kubernetes mgmtport-no-snat-subnets-v6 { type ipv6_addr ; flags interval ; comment "subnets not subject to management port SNAT (IPv6)" ; }
add chain inet ovn-kubernetes mgmtport-snat { type nat hook postrouting priority 100 ; comment "OVN SNAT to Management Port" ; }
add rule inet ovn-kubernetes mgmtport-snat oifname != %s return
add rule inet ovn-kubernetes mgmtport-snat meta nfproto ipv4 ip saddr 10.1.1.2 counter return
add rule inet ovn-kubernetes mgmtport-snat meta l4proto . th dport @mgmtport-no-snat-nodeports counter return
add rule inet ovn-kubernetes mgmtport-snat ip daddr . meta l4proto . th dport @mgmtport-no-snat-services-v4 counter return
add rule inet ovn-kubernetes mgmtport-snat ip saddr @mgmtport-no-snat-subnets-v4 counter return
add rule inet ovn-kubernetes mgmtport-snat counter snat ip to 10.1.1.2
`

// The base expected nftables rules with UDN enabled. You must substitute in the management port interface name.
const baseUDNNFTRulesFmt = `
add map inet ovn-kubernetes udn-mark-nodeports { type inet_proto . inet_service : verdict ; comment "UDN services NodePorts mark" ; }
add map inet ovn-kubernetes udn-mark-external-ips-v4 { type ipv4_addr . inet_proto . inet_service : verdict ; comment "UDN services External IPs mark (IPv4)" ; }
add map inet ovn-kubernetes udn-mark-external-ips-v6 { type ipv6_addr . inet_proto . inet_service : verdict ; comment "UDN services External IPs mark (IPv6)" ; }
add chain inet ovn-kubernetes udn-service-mark { comment "UDN services packet mark" ; }
add rule inet ovn-kubernetes udn-service-mark fib daddr type local meta l4proto . th dport vmap @udn-mark-nodeports
add rule inet ovn-kubernetes udn-service-mark ip daddr . meta l4proto . th dport vmap @udn-mark-external-ips-v4
add rule inet ovn-kubernetes udn-service-mark ip6 daddr . meta l4proto . th dport vmap @udn-mark-external-ips-v6
add chain inet ovn-kubernetes udn-service-prerouting { type filter hook prerouting priority -150 ; comment "UDN services packet mark - Prerouting" ; }
add rule inet ovn-kubernetes udn-service-prerouting iifname != %s jump udn-service-mark
add chain inet ovn-kubernetes udn-service-output { type filter hook output priority -150 ; comment "UDN services packet mark - Output" ; }
add rule inet ovn-kubernetes udn-service-output jump udn-service-mark
`

func getBaseNFTRules(mgmtPort string) string {
	ret := fmt.Sprintf(baseNFTRulesFmt, mgmtPort)
	if util.IsNetworkSegmentationSupportEnabled() {
		ret += fmt.Sprintf(baseUDNNFTRulesFmt, mgmtPort)
	}
	return ret
}

func shareGatewayInterfaceTest(app *cli.App, testNS ns.NetNS,
	eth0Name, eth0MAC, eth0GWIP, eth0CIDR string, gatewayVLANID uint, l netlink.Link, hwOffload, setNodeIP bool) {
	const mtu string = "1234"
	const clusterCIDR string = "10.1.0.0/16"
	config.Gateway.DisableForwarding = false

	var err error
	if len(eth0GWIP) > 0 {
		// And a default route
		err := testNS.Do(func(ns.NetNS) error {
			defRoute := &netlink.Route{
				LinkIndex: l.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
				Gw:        ovntest.MustParseIP(eth0GWIP),
			}
			return netlink.RouteAdd(defRoute)
		})
		Expect(err).NotTo(HaveOccurred())
	}

	app.Action = func(ctx *cli.Context) error {
		const (
			nodeName   string = "node1"
			systemID   string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
			nodeSubnet string = "10.1.1.0/24"
		)

		fexec := ovntest.NewLooseCompareFakeExec()

		// management port commands
		mpPortName := types.K8sMgmtIntfName
		mpPortRepName := types.K8sMgmtIntfName + "_0"
		mpPortLegacyName := types.K8sPrefix + nodeName
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mpPortName,
			Output: "internal," + mpPortName,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mpPortRepName,
			Output: "internal," + mpPortRepName,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 -- --if-exists del-port br-int " + mpPortLegacyName + " -- --may-exist add-port br-int " + mpPortName + " -- set interface " + mpPortName + " mac=\"0a:58:0a:01:01:02\" type=internal mtu_request=" + mtu + " external-ids:iface-id=" + mpPortLegacyName,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.forwarding=1",
			Output: "net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
		})

		// gateway commands
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 port-to-br eth0",
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 port-to-br eth0",
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 br-exists eth0",
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 -- --may-exist add-br breth0 -- br-set-external-id breth0 bridge-id breth0 -- br-set-external-id breth0 bridge-uplink eth0 -- set bridge breth0 fail-mode=standalone other_config:hwaddr=" + eth0MAC + " -- --may-exist add-port breth0 eth0 -- set port eth0 other-config:transient=true",
			Action: func() error {
				return testNS.Do(func(ns.NetNS) error {
					defer GinkgoRecover()
					_, err = netlink.LinkByName("br" + eth0Name)
					Expect(err).NotTo(HaveOccurred())
					return nil
				})
			},
		})
		if config.IPv4Mode {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "sysctl -w net.ipv4.conf.breth0.forwarding=1",
				Output: "net.ipv4.conf.breth0.forwarding = 1",
			})
		}

		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 mac_in_use",
			Output: eth0MAC,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
			Output: "",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":breth0",
		})

		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
			Output: systemID,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-appctl --timeout=15 dpif/show-dp-features breth0",
			Output: "Check pkt length action: Yes",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . other_config:hw-offload",
			Output: fmt.Sprintf("%t", hwOffload),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface patch-breth0_node1-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface eth0 ofport",
			Output: "7",
		})
		if setNodeIP {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 get Open_vSwitch . external_ids:ovn-encap-ip",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-encap-ip=192.168.1.10",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-appctl --timeout=5 -t ovn-controller exit --restart",
			})
		}
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip route replace table 7 172.16.1.0/24 via 10.1.1.1 dev ovn-k8s-mp0",
			Output: "0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip -4 rule",
			Output: "0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip -4 rule add fwmark 0x1745ec lookup 7 prio 30",
			Output: "0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.rp_filter=2",
			Output: "net.ipv4.conf.ovn-k8s-mp0.rp_filter = 2",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
		})
		// nodePortWatcher()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface eth0 ofport",
			Output: "7",
		})
		// syncServices()

		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		existingNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					// add some fake previous subnets to force OVNK to try to clean it
					util.OvnNodeMasqCIDR: "{\"ipv4\":\"170.254.0.0/16\",\"ipv6\":\"fa69::/112\"}",
				},
			},
		}
		if setNodeIP {
			expectedAddr, err := netlink.ParseAddr(eth0CIDR)
			Expect(err).NotTo(HaveOccurred())
			nodeAddr := corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: expectedAddr.IP.String()}
			existingNode.Status = corev1.NodeStatus{Addresses: []corev1.NodeAddress{nodeAddr}}
		}

		iptV4, iptV6 := util.SetFakeIPTablesHelpers()
		nft := nodenft.SetFakeNFTablesHelper()

		// Make Management port
		hostSubnets := ovntest.MustParseIPNets(nodeSubnet)
		rm := routemanager.NewController()
		netInfo := &multinetworkmocks.NetInfo{}
		netInfo.On("GetPodNetworkAdvertisedOnNodeVRFs", nodeName).Return(nil)
		mp, err := managementport.NewManagementPortController(&existingNode, hostSubnets, "", "", rm, netInfo)
		Expect(err).NotTo(HaveOccurred())

		kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
			Items: []corev1.Node{existingNode},
		})
		fakeClient := &util.OVNNodeClientset{
			KubeClient:            kubeFakeClient,
			NetworkAttchDefClient: nadfake.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		wg := &sync.WaitGroup{}
		defer func() {
			close(stop)
			wg.Wait()
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		k := &kube.Kube{KClient: kubeFakeClient}

		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)

		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(nodeSubnet))
		Expect(err).NotTo(HaveOccurred())
		err = nodeAnnotator.Run()
		Expect(err).NotTo(HaveOccurred())
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				rm.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			// create dummy management interface
			err := netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: types.K8sMgmtIntfName,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			// start management port
			err = mp.Start(stop)
			Expect(err).NotTo(HaveOccurred())

			// setup stale masquerade
			// Create breth0 as a dummy link
			err = netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name:         "br" + eth0Name,
					HardwareAddr: ovntest.MustParseMAC(eth0MAC),
				},
			})
			Expect(err).NotTo(HaveOccurred())
			link, err := netlink.LinkByName("br" + eth0Name)
			Expect(err).NotTo(HaveOccurred())
			err = netlink.LinkSetUp(link)
			Expect(err).NotTo(HaveOccurred())
			staleAddr, err := netlink.ParseAddr("170.254.0.2/32")
			Expect(err).NotTo(HaveOccurred())
			err = netlink.AddrAdd(link, staleAddr)
			Expect(err).NotTo(HaveOccurred())
			_, gw, err := net.ParseCIDR("170.254.0.1/32")
			Expect(err).NotTo(HaveOccurred())
			staleRoute := &netlink.Route{
				LinkIndex: link.Attrs().Index,
				Dst:       gw,
			}
			err = netlink.RouteAdd(staleRoute)
			Expect(err).NotTo(HaveOccurred())
			// ensure stale route is present
			r, err := util.LinkRouteGetFilteredRoute(
				staleRoute,
				netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_SRC,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(r).NotTo(BeNil())

			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())

			ifAddrs := ovntest.MustParseIPNets(eth0CIDR)
			sharedGw, err := newGateway(
				nodeName,
				ovntest.MustParseIPNets(nodeSubnet),
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotator,
				mp,
				k,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeShared,
			)
			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.initFunc()
			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.Init(stop, wg)
			Expect(err).NotTo(HaveOccurred())
			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			sharedGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			sharedGw.openflowManager.syncFlows()
			// Verify the code moved eth0's IP address, MAC, and routes
			// over to breth0
			l, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())
			addrs, err := netlink.AddrList(l, syscall.AF_INET)
			Expect(err).NotTo(HaveOccurred())
			var found, staleFound bool
			expectedAddr, err := netlink.ParseAddr(eth0CIDR)
			Expect(err).NotTo(HaveOccurred())
			for _, a := range addrs {
				// ensure stale masquerade IP was removed from the bridge
				if a.IP.Equal(staleAddr.IP) && bytes.Equal(a.Mask, staleAddr.Mask) {
					staleFound = true
				}
				// ensure code moved correct IP to bridge
				if a.IP.Equal(expectedAddr.IP) && bytes.Equal(a.Mask, expectedAddr.Mask) {
					found = true
				}
			}
			Expect(found).To(BeTrue())
			Expect(staleFound).To(BeFalse())

			Expect(l.Attrs().HardwareAddr.String()).To(Equal(eth0MAC))

			// check that the masquerade route was added
			expRoute := &netlink.Route{
				Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String())),
				LinkIndex: l.Attrs().Index,
				Src:       ifAddrs[0].IP,
			}
			Eventually(func() error {
				r, err := util.LinkRouteGetFilteredRoute(
					expRoute,
					netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_SRC,
				)
				if err != nil {
					return err
				}
				if r == nil {
					return fmt.Errorf("failed to find route")
				}
				return nil
			}, 1*time.Second).ShouldNot(HaveOccurred())
			// ensure stale masquerade route is no longer present
			r, err = util.LinkRouteGetFilteredRoute(
				staleRoute,
				netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_SRC,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(r).To(BeNil())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(fexec.CalledMatchesExpected, 5).Should(BeTrue(), fexec.ErrorDesc)
		// Make sure that annotation 'k8s.ovn.org/gateway-mtu-support' is set to "false" (because hw-offload is true).
		if hwOffload {
			Eventually(func() bool {
				node, err := kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), existingNode.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}
				return node.Annotations["k8s.ovn.org/gateway-mtu-support"] == "false"
			}, 5).Should(BeTrue(), "invalid annotation, hw-offload is enabled but annotation "+
				"'k8s.ovn.org/gateway-mtu-support' != \"false\"")
		} else {
			Consistently(func() bool {
				node, err := kubeFakeClient.CoreV1().Nodes().Get(context.TODO(), existingNode.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}
				return node.Annotations["k8s.ovn.org/gateway-mtu-support"] != "false"
			}, 5).Should(BeTrue(), "invalid annotation, hw-offload is disabled but found annotation "+
				"'k8s.ovn.org/gateway-mtu-support' with value == \"false\"")
		}

		expectedTables := map[string]util.FakeTable{
			"nat": {
				"PREROUTING": []string{
					"-j OVN-KUBE-ETP",
					"-j OVN-KUBE-EXTERNALIP",
					"-j OVN-KUBE-NODEPORT",
				},
				"OUTPUT": []string{
					"-j OVN-KUBE-EXTERNALIP",
					"-j OVN-KUBE-NODEPORT",
					"-j OVN-KUBE-ITP",
				},
				"OVN-KUBE-NODEPORT":   []string{},
				"OVN-KUBE-EXTERNALIP": []string{},
				"OVN-KUBE-ETP":        []string{},
				"OVN-KUBE-ITP":        []string{},
			},
			"filter": {},
			"mangle": {
				"OUTPUT": []string{
					"-j OVN-KUBE-ITP",
				},
				"OVN-KUBE-ITP": []string{},
			},
		}
		// OCP HACK: Block MCS Access. https://github.com/openshift/ovn-kubernetes/pull/170
		expectedMCSRules := []string{
			"-p tcp -m tcp --dport 22624 --syn -j REJECT",
			"-p tcp -m tcp --dport 22623 --syn -j REJECT",
		}
		expectedTables["filter"]["FORWARD"] = append(expectedMCSRules, expectedTables["filter"]["FORWARD"]...)
		expectedTables["filter"]["OUTPUT"] = append(expectedMCSRules, expectedTables["filter"]["OUTPUT"]...)
		// END OCP HACK
		f4 := iptV4.(*util.FakeIPTables)
		err = f4.MatchState(expectedTables, nil)
		Expect(err).NotTo(HaveOccurred())

		expectedTables = map[string]util.FakeTable{
			"nat":    {},
			"filter": {},
			"mangle": {},
		}
		f6 := iptV6.(*util.FakeIPTables)
		err = f6.MatchState(expectedTables, nil)
		Expect(err).NotTo(HaveOccurred())

		expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
		err = nodenft.MatchNFTRules(expectedNFT, nft.Dump())
		Expect(err).NotTo(HaveOccurred())

		// check that masquerade subnet annotation got updated
		node, err := wf.GetNode(nodeName)
		Expect(err).NotTo(HaveOccurred())
		subnets, err := util.ParseNodeMasqueradeSubnet(node)
		Expect(err).NotTo(HaveOccurred())
		for _, subnet := range subnets {
			if utilnet.IsIPv4CIDR(subnet) {
				Expect(subnet.String()).To(Equal(config.Gateway.V4MasqueradeSubnet))
			} else if utilnet.IsIPv6CIDR(subnet) {
				Expect(subnet.String()).To(Equal(config.Gateway.V6MasqueradeSubnet))
			}
		}

		return nil
	}

	err = app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
		"--init-gateways",
		"--gateway-interface=" + eth0Name,
		"--nodeport",
		"--gateway-vlanid=" + fmt.Sprintf("%d", gatewayVLANID),
		"--mtu=" + mtu,
	})
	Expect(err).NotTo(HaveOccurred())
}

func shareGatewayInterfaceDPUTest(app *cli.App, testNS ns.NetNS,
	brphys, hostMAC, hostCIDR, dpuIP string) {
	const mtu string = "1400"
	const clusterCIDR string = "10.1.0.0/16"
	app.Action = func(ctx *cli.Context) error {
		const (
			nodeName   string = "node1"
			systemID   string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
			nodeSubnet string = "10.1.1.0/24"
			uplinkPort string = "p0"
			uplinkMAC  string = "11:22:33:44:55:66"
			hostRep    string = "pf0hpf"
		)

		// sriovnet mocks
		sriovnetMock := &utilMock.SriovnetOps{}
		util.SetSriovnetOpsInst(sriovnetMock)
		sriovnetMock.On("GetRepresentorPortFlavour", hostRep).Return(sriovnet.PortFlavour(sriovnet.PORT_FLAVOUR_PCI_PF), nil)
		sriovnetMock.On("GetRepresentorPeerMacAddress", hostRep).Return(ovntest.MustParseMAC(hostMAC), nil)
		// exec Mocks
		fexec := ovntest.NewLooseCompareFakeExec()
		// gatewayInitInternal
		// bridgeForInterface
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 port-to-br " + brphys,
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 port-to-br " + brphys,
			Err:    fmt.Errorf(""),
			Output: brphys,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 br-exists " + brphys,
			Err: nil,
		})
		// getIntfName
		// GetNicName
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 list-ports " + brphys,
			Output: "p0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Port " + uplinkPort + " Interfaces",
			Output: "p0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface " + uplinkPort + " Type",
			Output: "system",
		})
		// getIntfName
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 get interface p0 ofport",
		})
		if config.IPv4Mode {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "sysctl -w net.ipv4.conf.brp0.forwarding=1",
				Output: "net.ipv4.conf.brp0.forwarding = 1",
			})
		}
		if config.IPv6Mode {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "sysctl -w net.ipv6.conf.brp0.forwarding=1",
				Output: "net.ipv6.conf.brp0.forwarding = 1",
			})
		}
		// bridgedGatewayNodeSetup
		// GetOVSPortMACAddress
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface " + brphys + " mac_in_use",
			Output: uplinkMAC,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
			Output: "",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":" + brphys,
		})
		// GetNodeChassisID
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
			Output: systemID,
		})
		// DetectCheckPktLengthSupport
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-appctl --timeout=15 dpif/show-dp-features " + brphys,
			Output: "Check pkt length action: Yes",
		})
		// IsOvsHwOffloadEnabled
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . other_config:hw-offload",
			Output: "false",
		})
		// GetDPUHostInterface
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 list-ports " + brphys,
			Output: hostRep,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Port " + hostRep + " Interfaces",
			Output: hostRep,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface " + hostRep + " Name",
			Output: hostRep,
		})
		// newGatewayOpenFlowManager
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface patch-" + brphys + "_node1-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface " + uplinkPort + " ofport",
			Output: "7",
		})
		// GetDPUHostInterface
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 list-ports " + brphys,
			Output: hostRep,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Port pf0hpf Interfaces",
			Output: hostRep,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface pf0hpf Name",
			Output: hostRep,
		})
		// newGatewayOpenFlowManager
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface " + hostRep + " ofport",
			Output: "9",
		})
		// cleanup flows
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-ofctl -O OpenFlow13 --bundle replace-flows " + brphys + " -",
		})
		// nodePortWatcher()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface " + uplinkPort + " ofport",
			Output: "7",
		})
		// syncServices()
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		nodeAddr := corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: dpuIP}
		existingNode := corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{nodeAddr}},
		}

		kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
			Items: []corev1.Node{existingNode},
		})
		fakeClient := &util.OVNNodeClientset{
			KubeClient:            kubeFakeClient,
			NetworkAttchDefClient: nadfake.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		wg := &sync.WaitGroup{}
		defer func() {
			close(stop)
			wg.Wait()
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		k := &kube.Kube{KClient: kubeFakeClient}

		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)

		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(nodeSubnet))
		Expect(err).NotTo(HaveOccurred())
		err = nodeAnnotator.Run()
		Expect(err).NotTo(HaveOccurred())

		ifAddrs := ovntest.MustParseIPNets(hostCIDR)
		ifAddrs[0].IP = ovntest.MustParseIP(dpuIP)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		rm := routemanager.NewController()
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				rm.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		// FIXME(mk): starting the gateway causing go routines to be spawned within sub functions and therefore they escape the
		// netns we wanted to set it to originally here. Refactor test cases to not spawn a go routine or just fake out everything
		// and remove need to create netns
		mpmock := &nodemocks.ManagementPort{}
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())
			sharedGw, err := newGateway(
				nodeName,
				ovntest.MustParseIPNets(nodeSubnet),
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotator,
				mpmock,
				k,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeShared,
			)
			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.initFunc()
			Expect(err).NotTo(HaveOccurred())
			err = sharedGw.Init(stop, wg)
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			//sharedGw.Start()
			sharedGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			sharedGw.openflowManager.syncFlows()

			// check that the masquerade route was not added
			l, err := netlink.LinkByName(brphys)
			Expect(err).NotTo(HaveOccurred())
			expRoute := &netlink.Route{
				Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String())),
				LinkIndex: l.Attrs().Index,
				Src:       ifAddrs[0].IP,
			}
			route, err := util.LinkRouteGetFilteredRoute(
				expRoute,
				netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_SRC,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(route).To(BeNil())

			return nil
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(fexec.CalledMatchesExpected, 5).Should(BeTrue(), fexec.ErrorDesc)

		// ensure correct l3 gw config were set in Node annotation
		updatedNode, err := fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		l3gwConfig, err := util.ParseNodeL3GatewayAnnotation(updatedNode)
		Expect(err).To(Not(HaveOccurred()))
		Expect(l3gwConfig.MACAddress.String()).To(Equal(hostMAC))
		Expect(l3gwConfig.IPAddresses[0].String()).To(Equal(ifAddrs[0].String()))
		return nil
	}

	err := app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
		"--init-gateways",
		"--gateway-interface=" + brphys,
		"--nodeport",
		"--mtu=" + mtu,
		"--ovnkube-node-mode=" + types.NodeModeDPU,
	})
	Expect(err).NotTo(HaveOccurred())
}

func shareGatewayInterfaceDPUHostTest(app *cli.App, testNS ns.NetNS, uplinkName, hostIP string) {
	const (
		clusterCIDR string = "10.1.0.0/16"
		svcCIDR     string = "172.16.1.0/24"
		nodeName    string = "node1"
	)

	app.Action = func(ctx *cli.Context) error {
		fexec := ovntest.NewLooseCompareFakeExec()
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		nodeAddr := corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: hostIP}
		existingNode := corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{nodeAddr}},
		}

		kubeFakeClient := fake.NewSimpleClientset(&corev1.NodeList{
			Items: []corev1.Node{existingNode},
		})
		fakeClient := &util.OVNNodeClientset{
			KubeClient:             kubeFakeClient,
			AdminPolicyRouteClient: adminpolicybasedrouteclient.NewSimpleClientset(),
			NetworkAttchDefClient:  nadfake.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		wg := &sync.WaitGroup{}
		defer func() {
			close(stop)
			wg.Wait()
			wf.Shutdown()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())
		ip, ipnet, err := net.ParseCIDR(hostIP + "/24")
		Expect(err).NotTo(HaveOccurred())
		ipnet.IP = ip
		routeManager := routemanager.NewController()
		cnnci := NewCommonNodeNetworkControllerInfo(kubeFakeClient, fakeClient.AdminPolicyRouteClient, wf, nil, nodeName, routeManager)
		nc := newDefaultNodeNetworkController(cnnci, stop, wg, routeManager, nil)
		// must run route manager manually which is usually started with nc.Start()
		wg.Add(1)
		go func() {
			defer GinkgoRecover()
			defer wg.Done()
			err := testNS.Do(func(ns.NetNS) error {
				nc.routeManager.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()

		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			err := nc.initGatewayDPUHost(net.ParseIP(hostIP))
			Expect(err).NotTo(HaveOccurred())

			link, err := netlink.LinkByName(uplinkName)
			Expect(err).NotTo(HaveOccurred())

			// check that the service route was added
			expRoute := &netlink.Route{
				Dst:       ovntest.MustParseIPNet(svcCIDR),
				LinkIndex: link.Attrs().Index,
				Gw:        ovntest.MustParseIP(config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP.String()),
			}
			Eventually(func() error {
				r, err := util.LinkRouteGetFilteredRoute(
					expRoute,
					netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_GW,
				)
				if err != nil {
					return err
				}
				if r == nil {
					return fmt.Errorf("failed to find route")
				}
				return nil
			}, 1*time.Second).ShouldNot(HaveOccurred())

			// check that the masquerade route was added
			expRoute = &netlink.Route{
				Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String())),
				LinkIndex: link.Attrs().Index,
				Src:       ovntest.MustParseIP(hostIP),
			}
			Eventually(func() error {
				r, err := util.LinkRouteGetFilteredRoute(
					expRoute,
					netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_GW,
				)
				if err != nil {
					return err
				}
				if r == nil {
					return fmt.Errorf("failed to find route")
				}
				return nil
			}, 1*time.Second).ShouldNot(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		return nil
	}

	err := app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
		"--init-gateways",
		"--gateway-interface=" + uplinkName,
		"--k8s-service-cidrs=" + svcCIDR,
		"--ovnkube-node-mode=dpu-host",
		"--ovnkube-node-mgmt-port-netdev=pf0vf0",
	})
	Expect(err).NotTo(HaveOccurred())
}

func localGatewayInterfaceTest(app *cli.App, testNS ns.NetNS,
	eth0Name, eth0MAC, eth0GWIP, eth0CIDR string, l netlink.Link) {
	const mtu string = "1234"
	const clusterCIDR string = "10.1.0.0/16"
	config.Gateway.DisableForwarding = true
	// Make this larger-than-default, so it makes sense for the UDN case
	config.Gateway.V4MasqueradeSubnet = "169.254.169.0/24"

	if len(eth0GWIP) > 0 {
		// And a default route
		err := testNS.Do(func(ns.NetNS) error {
			defRoute := &netlink.Route{
				LinkIndex: l.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
				Gw:        ovntest.MustParseIP(eth0GWIP),
			}
			return netlink.RouteAdd(defRoute)
		})
		Expect(err).NotTo(HaveOccurred())
	}

	app.Action = func(ctx *cli.Context) error {
		const (
			nodeName   string = "node1"
			systemID   string = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
			nodeSubnet string = "10.1.1.0/24"
		)

		ovsOFOutput := `
OFPT_FEATURES_REPLY (xid=0x2): dpid:00000242ac120002
n_tables:254, n_buffers:0
capabilities: FLOW_STATS TABLE_STATS PORT_STATS QUEUE_STATS ARP_MATCH_IP
actions: output enqueue set_vlan_vid set_vlan_pcp strip_vlan mod_dl_src mod_dl_dst mod_nw_src mod_nw_dst mod_nw_tos mod_tp_src mod_tp_dst
 1(eth0): addr:02:42:ac:12:00:02
     config:     0
     state:      0
     current:    10GB-FD COPPER
     speed: 10000 Mbps now, 0 Mbps max
 2(patch-breth0_ov): addr:8e:8d:f4:cd:4f:76
     config:     0
     state:      0
     speed: 0 Mbps now, 0 Mbps max
 LOCAL(breth0): addr:02:42:ac:12:00:02
     config:     0
     state:      0
     speed: 0 Mbps now, 0 Mbps max
OFPT_GET_CONFIG_REPLY (xid=0x4): frags=normal miss_send_len=0`

		fexec := ovntest.NewLooseCompareFakeExec()

		// management port commands
		mpPortName := types.K8sMgmtIntfName
		mpPortRepName := types.K8sMgmtIntfName + "_0"
		mpPortLegacyName := types.K8sPrefix + nodeName
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mpPortName,
			Output: "internal," + mpPortName,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mpPortRepName,
			Output: "internal," + mpPortRepName,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 -- --if-exists del-port br-int " + mpPortLegacyName + " -- --may-exist add-port br-int " + mpPortName + " -- set interface " + mpPortName + " mac=\"0a:58:0a:01:01:02\" type=internal mtu_request=" + mtu + " external-ids:iface-id=" + mpPortLegacyName,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.forwarding=1",
			Output: "net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
		})

		// gateway commands
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 port-to-br eth0",
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 port-to-br eth0",
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 br-exists eth0",
			Err: fmt.Errorf(""),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 -- --may-exist add-br breth0 -- br-set-external-id breth0 bridge-id breth0 -- br-set-external-id breth0 bridge-uplink eth0 -- set bridge breth0 fail-mode=standalone other_config:hwaddr=" + eth0MAC + " -- --may-exist add-port breth0 eth0 -- set port eth0 other-config:transient=true",
			Action: func() error {
				return testNS.Do(func(ns.NetNS) error {
					defer GinkgoRecover()

					// Create breth0 as a dummy link
					err := netlink.LinkAdd(&netlink.Dummy{
						LinkAttrs: netlink.LinkAttrs{
							Name:         "br" + eth0Name,
							HardwareAddr: ovntest.MustParseMAC(eth0MAC),
						},
					})
					Expect(err).NotTo(HaveOccurred())
					_, err = netlink.LinkByName("br" + eth0Name)
					Expect(err).NotTo(HaveOccurred())
					return nil
				})
			},
		})
		if config.IPv4Mode {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "sysctl -w net.ipv4.conf.breth0.forwarding=1",
				Output: "net.ipv4.conf.breth0.forwarding = 1",
			})
		}

		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface breth0 mac_in_use",
			Output: eth0MAC,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
			Output: "",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-bridge-mappings=" + types.PhysicalNetworkName + ":breth0",
		})

		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
			Output: systemID,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-appctl --timeout=15 dpif/show-dp-features breth0",
			Output: "Check pkt length action: Yes",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . other_config:hw-offload",
			Output: "false",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Interface patch-breth0_node1-to-br-int ofport",
			Output: "5",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get interface eth0 ofport",
			Output: "7",
		})
		// IP already configured, do not try to set it or restart ovn-controller
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 get Open_vSwitch . external_ids:ovn-encap-ip",
			Output: "192.168.1.10",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip route replace table 7 172.16.1.0/24 via 10.1.1.1 dev ovn-k8s-mp0",
			Output: "0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip -4 rule",
			Output: "0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ip -4 rule add fwmark 0x1745ec lookup 7 prio 30",
			Output: "0",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.rp_filter=2",
			Output: "net.ipv4.conf.ovn-k8s-mp0.rp_filter = 2",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-ofctl show breth0",
			Output: ovsOFOutput,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface eth0 ofport",
			Output: "7",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-ofctl show breth0",
			Output: ovsOFOutput,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovs-ofctl -O OpenFlow13 --bundle replace-flows breth0 -",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovs-ofctl show breth0",
			Output: ovsOFOutput,
		})
		// syncServices()

		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		_, err = config.InitConfig(ctx, fexec, nil)
		Expect(err).NotTo(HaveOccurred())

		expectedAddr, err := netlink.ParseAddr(eth0CIDR)
		Expect(err).NotTo(HaveOccurred())
		nodeAddr := corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: expectedAddr.IP.String()}
		existingNode := corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{nodeAddr}},
		}
		externalIP := "1.1.1.1"
		externalIPPort := int32(8032)
		service := *newService("service1", "namespace1", "10.129.0.2",
			[]corev1.ServicePort{
				{
					Port:     externalIPPort,
					Protocol: corev1.ProtocolTCP,
				},
			},
			corev1.ServiceTypeClusterIP,
			[]string{externalIP},
			corev1.ServiceStatus{},
			false, false,
		)
		endpointSlice := *newEndpointSlice("service1", "namespace1", []discovery.Endpoint{}, []discovery.EndpointPort{})

		nft := nodenft.SetFakeNFTablesHelper()

		// Make Management port
		hostSubnets := ovntest.MustParseIPNets(nodeSubnet)
		rm := routemanager.NewController()
		netInfo := &multinetworkmocks.NetInfo{}
		netInfo.On("GetPodNetworkAdvertisedOnNodeVRFs", nodeName).Return(nil)
		mp, err := managementport.NewManagementPortController(&existingNode, hostSubnets, "", "", rm, netInfo)
		Expect(err).NotTo(HaveOccurred())

		if util.IsNetworkSegmentationSupportEnabled() {
			err = configureUDNServicesNFTables()
			Expect(err).NotTo(HaveOccurred())
		}

		kubeFakeClient := fake.NewSimpleClientset(
			&corev1.NodeList{
				Items: []corev1.Node{existingNode},
			},
			&service,
			&endpointSlice,
		)
		fakeClient := &util.OVNNodeClientset{
			KubeClient:               kubeFakeClient,
			NetworkAttchDefClient:    nadfake.NewSimpleClientset(),
			UserDefinedNetworkClient: udnfakeclient.NewSimpleClientset(),
		}

		stop := make(chan struct{})
		wf, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		wg := &sync.WaitGroup{}
		defer func() {
			close(stop)
			wf.Shutdown()
			wg.Wait()
		}()
		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		k := &kube.Kube{KClient: kubeFakeClient}
		iptV4, iptV6 := util.SetFakeIPTablesHelpers()

		nodeAnnotator := kube.NewNodeAnnotator(k, existingNode.Name)

		err = util.SetNodeHostSubnetAnnotation(nodeAnnotator, ovntest.MustParseIPNets(nodeSubnet))
		Expect(err).NotTo(HaveOccurred())
		err = nodeAnnotator.Run()
		Expect(err).NotTo(HaveOccurred())
		ip, ipNet, _ := net.ParseCIDR(eth0CIDR)
		ipNet.IP = ip
		go func() {
			defer GinkgoRecover()
			err := testNS.Do(func(ns.NetNS) error {
				rm.Run(stop, 10*time.Second)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		}()
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			// create dummy management interface
			err := netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: types.K8sMgmtIntfName,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			// start management port
			err = mp.Start(stop)
			Expect(err).NotTo(HaveOccurred())

			Expect(configureGlobalForwarding()).To(Succeed())
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())
			ifAddrs := ovntest.MustParseIPNets(eth0CIDR)
			localGw, err := newGateway(
				nodeName,
				ovntest.MustParseIPNets(nodeSubnet),
				gatewayNextHops,
				gatewayIntf,
				"",
				ifAddrs,
				nodeAnnotator,
				mp,
				k,
				wf,
				rm,
				nil,
				networkmanager.Default().Interface(),
				config.GatewayModeLocal,
			)
			Expect(err).NotTo(HaveOccurred())
			err = localGw.initFunc()
			Expect(err).NotTo(HaveOccurred())
			err = localGw.Init(stop, wg)
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			// we cannot start the shared gw directly because it will spawn a goroutine that may not be bound to the test netns
			// Start does two things, starts nodeIPManager which spawns a go routine and also starts openflow manager by spawning a go routine
			// localGw.Start()
			localGw.nodeIPManager.sync()
			// we cannot start openflow manager directly because it spawns a go routine
			// FIXME: extract openflow manager func from the spawning of a go routine so it can be called directly below.
			localGw.openflowManager.syncFlows()

			// Verify the code moved eth0's IP address, MAC, and routes
			// over to breth0
			l, err := netlink.LinkByName("breth0")
			Expect(err).NotTo(HaveOccurred())
			addrs, err := netlink.AddrList(l, syscall.AF_INET)
			Expect(err).NotTo(HaveOccurred())
			var found bool
			expectedAddr, err := netlink.ParseAddr(eth0CIDR)
			Expect(err).NotTo(HaveOccurred())
			for _, a := range addrs {
				if a.IP.Equal(expectedAddr.IP) && bytes.Equal(a.Mask, expectedAddr.Mask) {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())

			Expect(l.Attrs().HardwareAddr.String()).To(Equal(eth0MAC))

			// check that the masquerade route was added
			expRoute := &netlink.Route{
				Dst:       ovntest.MustParseIPNet(fmt.Sprintf("%s/32", config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String())),
				LinkIndex: l.Attrs().Index,
				Src:       ifAddrs[0].IP,
			}
			Eventually(func() error {
				r, err := util.LinkRouteGetFilteredRoute(
					expRoute,
					netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF|netlink.RT_FILTER_SRC,
				)
				if err != nil {
					return err
				}
				if r == nil {
					return fmt.Errorf("failed to find route")
				}
				return nil
			}, 1*time.Second).ShouldNot(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Eventually(fexec.CalledMatchesExpected, 5).Should(BeTrue(), fexec.ErrorDesc)

		expectedTables := map[string]util.FakeTable{
			"nat": {
				"PREROUTING": []string{
					"-j OVN-KUBE-ETP",
					"-j OVN-KUBE-EXTERNALIP",
					"-j OVN-KUBE-NODEPORT",
				},
				"OUTPUT": []string{
					"-j OVN-KUBE-EXTERNALIP",
					"-j OVN-KUBE-NODEPORT",
					"-j OVN-KUBE-ITP",
				},
				"OVN-KUBE-NODEPORT": []string{},
				"OVN-KUBE-EXTERNALIP": []string{
					fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
				},
				"POSTROUTING": []string{
					"-s 169.254.169.1 -j MASQUERADE",
					"-s 10.1.1.0/24 -j MASQUERADE",
				},
				"OVN-KUBE-ETP": []string{},
				"OVN-KUBE-ITP": []string{},
			},
			"filter": {
				"FORWARD": []string{
					"-d 169.254.169.1 -j ACCEPT",
					"-s 169.254.169.1 -j ACCEPT",
					"-d 172.16.1.0/24 -j ACCEPT",
					"-s 172.16.1.0/24 -j ACCEPT",
					"-d 10.1.0.0/16 -j ACCEPT",
					"-s 10.1.0.0/16 -j ACCEPT",
					"-i ovn-k8s-mp0 -j ACCEPT",
					"-o ovn-k8s-mp0 -j ACCEPT",
				},
				"INPUT": []string{
					"-i ovn-k8s-mp0 -m comment --comment from OVN to localhost -j ACCEPT",
				},
			},
			"mangle": {
				"OUTPUT": []string{
					"-j OVN-KUBE-ITP",
				},
				"OVN-KUBE-ITP": []string{},
			},
		}
		// OCP HACK: Block MCS Access. https://github.com/openshift/ovn-kubernetes/pull/170
		expectedMCSRules := []string{
			"-p tcp -m tcp --dport 22624 --syn -j REJECT",
			"-p tcp -m tcp --dport 22623 --syn -j REJECT",
		}
		expectedTables["filter"]["FORWARD"] = append(expectedMCSRules, expectedTables["filter"]["FORWARD"]...)
		expectedTables["filter"]["OUTPUT"] = append(expectedMCSRules, expectedTables["filter"]["OUTPUT"]...)
		// END OCP HACK
		if util.IsNetworkSegmentationSupportEnabled() {
			expectedTables["nat"]["POSTROUTING"] = append(expectedTables["nat"]["POSTROUTING"],
				"-j OVN-KUBE-UDN-MASQUERADE",
			)
			expectedTables["nat"]["OVN-KUBE-UDN-MASQUERADE"] = append(expectedTables["nat"]["OVN-KUBE-UDN-MASQUERADE"],
				"-s 169.254.169.0/29 -j RETURN",     // this guarantees we don't SNAT default network masqueradeIPs
				"-d 172.16.1.0/24 -j RETURN",        // this guarantees we don't SNAT service traffic
				"-s 169.254.169.0/24 -j MASQUERADE", // this guarantees we SNAT all UDN MasqueradeIPs traffic leaving the node
			)
		}
		f4 := iptV4.(*util.FakeIPTables)
		err = f4.MatchState(expectedTables, map[util.FakePolicyKey]string{{
			Table: "filter",
			Chain: "FORWARD",
		}: "DROP"})
		Expect(err).NotTo(HaveOccurred())

		expectedTables = map[string]util.FakeTable{
			"nat":    {},
			"filter": {},
			"mangle": {},
		}
		f6 := iptV6.(*util.FakeIPTables)
		err = f6.MatchState(expectedTables, nil)
		Expect(err).NotTo(HaveOccurred())

		expectedNFT := getBaseNFTRules(types.K8sMgmtIntfName)
		err = nodenft.MatchNFTRules(expectedNFT, nft.Dump())
		Expect(err).NotTo(HaveOccurred())

		return nil
	}

	err := app.Run([]string{
		app.Name,
		"--cluster-subnets=" + clusterCIDR,
		"--gateway-mode=local",
		"--gateway-interface=" + eth0Name,
		"--nodeport",
		"--mtu=" + mtu,
	})
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("Gateway Init Operations", func() {

	var (
		testNS ns.NetNS
		app    *cli.App
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		var err error
		runtime.LockOSThread()
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(testNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(testNS)).To(Succeed())
		runtime.UnlockOSThread()
	})

	Context("Setting up the gateway bridge", func() {
		const (
			eth0Name string = "eth0"
			eth0IP   string = "192.168.1.10"
			eth0CIDR string = eth0IP + "/24"
			eth0GWIP string = "192.168.1.1"
		)
		var eth0MAC string
		var link netlink.Link

		BeforeEach(func() {
			// Set up a fake eth0
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				ovntest.AddLink(eth0Name)

				var err error
				link, err = netlink.LinkByName(eth0Name)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(link)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(eth0CIDR)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.AddrAdd(link, addr)
				Expect(err).NotTo(HaveOccurred())

				eth0MAC = link.Attrs().HardwareAddr.String()

				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		ovntest.OnSupportedPlatformsIt("sets up a local gateway with predetermined interface", func() {
			localGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, link)
		})

		ovntest.OnSupportedPlatformsIt("sets up a local gateway with predetermined interface when network-segmentation is enabled", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			localGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, link)
		})

		ovntest.OnSupportedPlatformsIt("sets up a local gateway with predetermined interface and no default route", func() {
			localGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, "", eth0CIDR, link)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 0, link, false, true)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with hw-offloading", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 0, link, true, true)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with tagged VLAN", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 3000, link, false, true)
		})

		config.Gateway.Interface = eth0Name
		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with predetermined gateway interface", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 0, link, false, true)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with tagged VLAN + predetermined gateway interface", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 3000, link, false, true)
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with predetermined gateway interface and no default route", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, "", eth0CIDR, 0, link, false, true)
		})

		// don't set the node status internal IP, addMasqueradeRoute will
		// fallback to the provided interface IP
		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway with node status internal IPs unset", func() {
			shareGatewayInterfaceTest(app, testNS, eth0Name, eth0MAC, eth0GWIP, eth0CIDR, 0, link, false, false)
		})
	})
})

var _ = Describe("Gateway Operations DPU", func() {
	var (
		testNS ns.NetNS
		app    *cli.App
	)

	BeforeEach(func() {
		var err error
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())

		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		_, _ = util.SetFakeIPTablesHelpers()
	})

	AfterEach(func() {
		Expect(testNS.Close()).To(Succeed())
	})

	Context("DPU Operations", func() {
		const (
			brphys   string = "brp0"
			dpuIP    string = "192.168.1.101"
			hostIP   string = "192.168.1.10"
			hostMAC  string = "aa:bb:cc:dd:ee:ff"
			hostCIDR string = hostIP + "/24"
			dpuCIDR  string = dpuIP + "/24"
			gwIP     string = "192.168.1.1"
		)

		BeforeEach(func() {
			// Create "bridge interface"
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				ovntest.AddLink(brphys)
				l, err := netlink.LinkByName(brphys)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(l)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(dpuCIDR)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.AddrAdd(l, addr)
				Expect(err).NotTo(HaveOccurred())

				// And a default route
				err = netlink.RouteAdd(&netlink.Route{
					LinkIndex: l.Attrs().Index,
					Scope:     netlink.SCOPE_UNIVERSE,
					Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
					Gw:        ovntest.MustParseIP(gwIP),
				})
				Expect(err).NotTo(HaveOccurred())

				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway DPU", func() {
			shareGatewayInterfaceDPUTest(app, testNS, brphys, hostMAC, hostCIDR, dpuIP)
		})
	})

	Context("DPU Host Operations", func() {
		const (
			uplinkName string = "enp3s0f0"
			hostIP     string = "192.168.1.10"
			hostCIDR   string = hostIP + "/24"
			gwIP       string = "192.168.1.1"
		)

		BeforeEach(func() {
			// Create "uplink" interface
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				ovntest.AddLink(uplinkName)
				l, err := netlink.LinkByName(uplinkName)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.LinkSetUp(l)
				Expect(err).NotTo(HaveOccurred())

				// Add an IP address
				addr, err := netlink.ParseAddr(hostCIDR)
				Expect(err).NotTo(HaveOccurred())
				err = netlink.AddrAdd(l, addr)
				Expect(err).NotTo(HaveOccurred())

				// And a default route
				err = netlink.RouteAdd(&netlink.Route{
					LinkIndex: l.Attrs().Index,
					Scope:     netlink.SCOPE_UNIVERSE,
					Dst:       ovntest.MustParseIPNet("0.0.0.0/0"),
					Gw:        ovntest.MustParseIP(gwIP),
				})
				Expect(err).NotTo(HaveOccurred())
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		ovntest.OnSupportedPlatformsIt("sets up a shared interface gateway DPU host", func() {
			shareGatewayInterfaceDPUHostTest(app, testNS, uplinkName, hostIP)
		})
	})
})

var _ = Describe("Gateway unit tests", func() {
	var netlinkMock *utilMock.NetLinkOps
	origNetlinkInst := util.GetNetLinkOps()

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		netlinkMock = &utilMock.NetLinkOps{}
		util.SetNetLinkOpMockInst(netlinkMock)
	})

	AfterEach(func() {
		util.SetNetLinkOpMockInst(origNetlinkInst)
	})

	Context("getDPUHostPrimaryIPAddresses", func() {

		It("returns Gateway IP/Subnet for kubernetes node IP", func() {
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.0.11")
			expectedGwSubnet := []*net.IPNet{
				{IP: nodeIP, Mask: net.CIDRMask(24, 32)},
			}
			gwSubnet, err := getDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).ToNot(HaveOccurred())
			Expect(gwSubnet).To(Equal(expectedGwSubnet))
		})

		It("Fails if node IP is not in host subnets", func() {
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.1.11")
			_, err := getDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).To(HaveOccurred())
		})

		It("returns node IP with config.Gateway.RouterSubnet subnet", func() {
			config.Gateway.RouterSubnet = "10.1.0.0/16"
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.1.0.11")
			expectedGwSubnet := []*net.IPNet{
				{IP: nodeIP, Mask: net.CIDRMask(16, 32)},
			}
			gwSubnet, err := getDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).ToNot(HaveOccurred())
			Expect(gwSubnet).To(Equal(expectedGwSubnet))
		})

		It("Fails if node IP is not in config.Gateway.RouterSubnet subnet", func() {
			config.Gateway.RouterSubnet = "10.1.0.0/16"
			_, dpuSubnet, _ := net.ParseCIDR("10.0.0.101/24")
			nodeIP := net.ParseIP("10.0.0.11")
			_, err := getDPUHostPrimaryIPAddresses(nodeIP, []*net.IPNet{dpuSubnet})
			Expect(err).To(HaveOccurred())
		})
	})

	Context("getInterfaceByIP", func() {
		It("Finds correct interface", func() {
			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name: "ens1f0",
			}
			lnkIpnet1 := &net.IPNet{
				IP:   net.ParseIP("10.0.0.11"),
				Mask: net.CIDRMask(24, 32),
			}
			lnkIpnet2 := &net.IPNet{
				IP:   net.ParseIP("10.0.0.12"),
				Mask: net.CIDRMask(24, 32),
			}
			addrs := []netlink.Addr{{IPNet: lnkIpnet1}, {IPNet: lnkIpnet2}}
			lnk.On("Attrs").Return(lnkAttr)
			netlinkMock.On("LinkList").Return([]netlink.Link{lnk}, nil)
			netlinkMock.On("LinkByName", lnkAttr.Name).Return(lnk, nil)
			netlinkMock.On("AddrList", lnk, mock.Anything).Return(addrs, nil)

			iface, err := getInterfaceByIP(net.ParseIP("10.0.0.12"))
			Expect(err).ToNot(HaveOccurred())
			Expect(iface).To(Equal(lnkAttr.Name))
		})

		It("Fails if interface not found", func() {
			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name: "ens1f0",
			}
			lnkIpnet1 := &net.IPNet{
				IP:   net.ParseIP("10.0.0.11"),
				Mask: net.CIDRMask(24, 32),
			}
			lnkIpnet2 := &net.IPNet{
				IP:   net.ParseIP("10.0.0.12"),
				Mask: net.CIDRMask(24, 32),
			}
			addrs := []netlink.Addr{{IPNet: lnkIpnet1}, {IPNet: lnkIpnet2}}
			lnk.On("Attrs").Return(lnkAttr)
			netlinkMock.On("LinkList").Return([]netlink.Link{lnk}, nil)
			netlinkMock.On("LinkByName", lnkAttr.Name).Return(lnk, nil)
			netlinkMock.On("AddrList", lnk, mock.Anything).Return(addrs, nil)

			_, err := getInterfaceByIP(net.ParseIP("10.0.1.12"))
			Expect(err).To(HaveOccurred())
		})

		It("Fails if link list call fails", func() {
			netlinkMock.On("LinkList").Return(nil, fmt.Errorf("failed to list links"))

			_, err := getInterfaceByIP(net.ParseIP("10.0.1.12"))
			Expect(err).To(HaveOccurred())
		})
	})

	Context("configureSvcRouteViaInterface", func() {

		It("Configures kubernetes service routes on interface", func() {
			_, ipnet, err := net.ParseCIDR("10.96.0.0/16")
			Expect(err).ToNot(HaveOccurred())
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{ipnet}
			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}
			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name:  "ens1f0",
				Index: 5,
			}
			lnk.On("Attrs").Return(lnkAttr)
			netlinkMock.On("LinkByName", lnkAttr.Name).Return(lnk, nil)
			netlinkMock.On("LinkByIndex", lnkAttr.Index).Return(lnk, nil)
			netlinkMock.On("LinkSetUp", mock.Anything).Return(nil)
			netlinkMock.On("RouteReplace", mock.Anything, mock.Anything, mock.Anything).Return(nil)
			wg := &sync.WaitGroup{}
			rm := routemanager.NewController()
			util.SetNetLinkOpMockInst(netlinkMock)
			stopCh := make(chan struct{})
			wg.Add(1)
			go func() {
				rm.Run(stopCh, 10*time.Second)
				wg.Done()
			}()
			defer func() {
				close(stopCh)
				wg.Wait()
			}()
			err = configureSvcRouteViaInterface(rm, "ens1f0", gwIPs)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Replaces previous kubernetes service routes on interface when MTU changes", func() {
			_, ipnet, err := net.ParseCIDR("10.96.0.0/16")
			Expect(err).ToNot(HaveOccurred())
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{ipnet}
			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}
			srcIP := config.Gateway.MasqueradeIPs.V4HostMasqueradeIP
			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name:  "ens1f0",
				Index: 5,
			}
			expectedRoute := &netlink.Route{
				Dst:       ipnet,
				LinkIndex: 5,
				Scope:     netlink.SCOPE_UNIVERSE,
				Gw:        gwIPs[0],
				MTU:       config.Default.MTU,
				Src:       srcIP,
				Table:     syscall.RT_TABLE_MAIN,
			}

			lnk.On("Attrs").Return(lnkAttr)
			netlinkMock.On("LinkByName", lnkAttr.Name).Return(lnk, nil)
			netlinkMock.On("LinkByIndex", lnkAttr.Index).Return(lnk, nil)
			netlinkMock.On("LinkSetUp", mock.Anything).Return(nil)
			netlinkMock.On("RouteReplace", expectedRoute).Return(nil)
			wg := &sync.WaitGroup{}
			rm := routemanager.NewController()
			util.SetNetLinkOpMockInst(netlinkMock)
			stopCh := make(chan struct{})
			wg.Add(1)
			go func() {
				rm.Run(stopCh, 10*time.Second)
				wg.Done()
			}()

			defer func() {
				close(stopCh)
				wg.Wait()
			}()

			err = configureSvcRouteViaInterface(rm, "ens1f0", gwIPs)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Fails if link set up fails", func() {
			netlinkMock.On("LinkByName", mock.Anything).Return(nil, fmt.Errorf("failed to find interface"))
			netlinkMock.On("LinkByIndex", mock.Anything).Return(nil, fmt.Errorf("failed to find interface"))
			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}
			wg := &sync.WaitGroup{}
			rm := routemanager.NewController()
			util.SetNetLinkOpMockInst(netlinkMock)
			stopCh := make(chan struct{})
			wg.Add(1)
			go func() {
				rm.Run(stopCh, 10*time.Second)
				wg.Done()
			}()
			defer func() {
				close(stopCh)
				wg.Wait()
			}()
			err := configureSvcRouteViaInterface(rm, "ens1f0", gwIPs)
			Expect(err).To(HaveOccurred())
		})

		It("Fails if IP family missmatch", func() {
			_, ipnet, err := net.ParseCIDR("fc00:123:456:15::/64")
			Expect(err).ToNot(HaveOccurred())
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{ipnet}

			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}
			netlinkMock.On("LinkByName", mock.Anything).Return(nil, nil)
			netlinkMock.On("LinkSetUp", mock.Anything).Return(nil)
			wg := &sync.WaitGroup{}
			rm := routemanager.NewController()
			util.SetNetLinkOpMockInst(netlinkMock)
			stopCh := make(chan struct{})
			wg.Add(1)
			go func() {
				rm.Run(stopCh, 10*time.Second)
				wg.Done()
			}()
			defer func() {
				close(stopCh)
				wg.Wait()
			}()
			err = configureSvcRouteViaInterface(rm, "ens1f0", gwIPs)
			Expect(err).To(HaveOccurred())
		})
	})

	Context("getGatewayNextHops", func() {

		It("Finds correct gateway interface and nexthops without configuration", func() {
			_, ipnet, err := net.ParseCIDR("0.0.0.0/0")
			Expect(err).ToNot(HaveOccurred())
			config.Kubernetes.ServiceCIDRs = []*net.IPNet{ipnet}
			gwIPs := []net.IP{net.ParseIP("10.0.0.11")}
			lnk := &linkMock.Link{}
			lnkAttr := &netlink.LinkAttrs{
				Name:  "ens1f0",
				Index: 5,
			}
			defaultRoute := &netlink.Route{
				Dst:       ipnet,
				LinkIndex: 5,
				Scope:     netlink.SCOPE_UNIVERSE,
				Gw:        gwIPs[0],
				MTU:       config.Default.MTU,
			}
			lnk.On("Attrs").Return(lnkAttr)
			netlinkMock.On("LinkByName", mock.Anything).Return(lnk, nil)
			netlinkMock.On("LinkByIndex", mock.Anything).Return(lnk, nil)
			netlinkMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return([]netlink.Route{*defaultRoute}, nil)
			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())
			Expect(gatewayIntf).To(Equal(lnkAttr.Name))
			Expect(gatewayNextHops[0]).To(Equal(gwIPs[0]))
		})

		It("Finds correct gateway interface and nexthops with single stack configuration", func() {
			ifName := "enf1f0"
			nextHopCfg := "10.0.0.11"

			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 port-to-br %s", ifName),
				Err: fmt.Errorf(""),
			})
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			gwIPs := []net.IP{net.ParseIP(nextHopCfg)}
			config.Gateway.Interface = ifName
			config.Gateway.NextHop = nextHopCfg

			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())
			Expect(gatewayIntf).To(Equal(ifName))
			Expect(gatewayNextHops[0]).To(Equal(gwIPs[0]))
		})

		It("Finds correct gateway interface and nexthops with dual stack configuration", func() {
			ifName := "enf1f0"
			nextHopCfg := "10.0.0.11,fc00:f853:ccd:e793::1"

			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 port-to-br %s", ifName),
				Err: fmt.Errorf(""),
			})
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			nextHops := strings.Split(nextHopCfg, ",")
			gwIPs := []net.IP{net.ParseIP(nextHops[0]), net.ParseIP(nextHops[1])}
			config.Gateway.Interface = ifName
			config.Gateway.NextHop = nextHopCfg
			config.IPv4Mode = true
			config.IPv6Mode = true

			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())
			Expect(gatewayIntf).To(Equal(ifName))
			Expect(gatewayNextHops).To(Equal(gwIPs))
		})

		ovntest.OnSupportedPlatformsIt("Finds correct gateway interface and nexthops when gateway bridge is created", func() {
			ifName := "enf1f0"
			nextHopCfg := "10.0.0.11"

			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    fmt.Sprintf("ovs-vsctl --timeout=15 port-to-br %s", ifName),
				Err:    fmt.Errorf(""),
				Output: "br" + ifName,
			})
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			gwIPs := []net.IP{net.ParseIP(nextHopCfg)}
			config.Gateway.Interface = ifName
			config.Gateway.NextHop = nextHopCfg

			gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
			Expect(err).NotTo(HaveOccurred())
			Expect(gatewayIntf).To(Equal(ifName))
			Expect(gatewayNextHops[0]).To(Equal(gwIPs[0]))
		})

		Context("In Local GW mode", func() {
			ovntest.OnSupportedPlatformsIt("Finds correct gateway interface and nexthops when dummy gateway bridge is created", func() {
				ifName := "enf1f0"
				dummyBridgeName := "br-ex"
				_, ipnet, err := net.ParseCIDR("0.0.0.0/0")
				Expect(err).ToNot(HaveOccurred())
				hostGwIPs := []net.IP{net.ParseIP("10.0.0.11")}
				lnk := &linkMock.Link{}
				lnkAttr := &netlink.LinkAttrs{
					Name:  ifName,
					Index: 5,
				}
				defaultRoute := &netlink.Route{
					Dst:       ipnet,
					LinkIndex: 5,
					Scope:     netlink.SCOPE_UNIVERSE,
					Gw:        hostGwIPs[0],
					MTU:       config.Default.MTU,
				}
				lnk.On("Attrs").Return(lnkAttr)
				netlinkMock.On("LinkByName", mock.Anything).Return(lnk, nil)
				netlinkMock.On("LinkByIndex", mock.Anything).Return(lnk, nil)
				netlinkMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return([]netlink.Route{*defaultRoute}, nil)

				fexec := ovntest.NewLooseCompareFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    fmt.Sprintf("ovs-vsctl --timeout=15 port-to-br %s", ifName),
					Err:    fmt.Errorf(""),
					Output: "",
				})
				err = util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				gwIPs := []net.IP{config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP}
				config.Gateway.Interface = dummyBridgeName
				config.Gateway.Mode = config.GatewayModeLocal
				config.Gateway.AllowNoUplink = true

				gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
				Expect(err).NotTo(HaveOccurred())
				Expect(gatewayIntf).To(Equal(dummyBridgeName))
				Expect(gatewayNextHops[0]).To(Equal(gwIPs[0]))
			})

			ovntest.OnSupportedPlatformsIt("Finds correct gateway interface and nexthops when dummy gateway bridge is created and no default route", func() {
				ifName := "enf1f0"
				dummyBridgeName := "br-ex"
				lnk := &linkMock.Link{}
				lnkAttr := &netlink.LinkAttrs{
					Name:  ifName,
					Index: 5,
				}

				lnk.On("Attrs").Return(lnkAttr)
				netlinkMock.On("LinkByName", mock.Anything).Return(lnk, nil)
				netlinkMock.On("LinkByIndex", mock.Anything).Return(lnk, nil)
				netlinkMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return([]netlink.Route{}, nil)

				fexec := ovntest.NewLooseCompareFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    fmt.Sprintf("ovs-vsctl --timeout=15 port-to-br %s", ifName),
					Err:    fmt.Errorf(""),
					Output: "",
				})
				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				gwIPs := []net.IP{config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP}
				config.Gateway.Interface = dummyBridgeName
				config.Gateway.Mode = config.GatewayModeLocal

				gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
				Expect(err).NotTo(HaveOccurred())
				Expect(gatewayIntf).To(Equal(dummyBridgeName))
				Expect(gatewayNextHops[0]).To(Equal(gwIPs[0]))
			})

			ovntest.OnSupportedPlatformsIt("Returns error when dummy gateway bridge is created without allow-no-uplink flag", func() {
				ifName := "enf1f0"
				dummyBridgeName := "br-ex"
				_, ipnet, err := net.ParseCIDR("0.0.0.0/0")
				Expect(err).ToNot(HaveOccurred())
				hostGwIPs := []net.IP{net.ParseIP("10.0.0.11")}
				lnk := &linkMock.Link{}
				lnkAttr := &netlink.LinkAttrs{
					Name:  ifName,
					Index: 5,
				}
				defaultRoute := &netlink.Route{
					Dst:       ipnet,
					LinkIndex: 5,
					Scope:     netlink.SCOPE_UNIVERSE,
					Gw:        hostGwIPs[0],
					MTU:       config.Default.MTU,
				}
				lnk.On("Attrs").Return(lnkAttr)
				netlinkMock.On("LinkByName", mock.Anything).Return(lnk, nil)
				netlinkMock.On("LinkByIndex", mock.Anything).Return(lnk, nil)
				netlinkMock.On("RouteListFiltered", mock.Anything, mock.Anything, mock.Anything).Return([]netlink.Route{*defaultRoute}, nil)

				fexec := ovntest.NewLooseCompareFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    fmt.Sprintf("ovs-vsctl --timeout=15 port-to-br %s", ifName),
					Err:    fmt.Errorf(""),
					Output: "",
				})
				err = util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				config.Gateway.Interface = dummyBridgeName
				config.Gateway.Mode = config.GatewayModeLocal

				gatewayNextHops, gatewayIntf, err := getGatewayNextHops()
				Expect(errors.As(err, new(*GatewayInterfaceMismatchError))).To(BeTrue())
				Expect(gatewayIntf).To(Equal(""))
				Expect(gatewayNextHops).To(BeEmpty())
			})
		})
	})
})
