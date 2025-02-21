
package main


import (
	"encoding/binary"
	"fmt"
	"net"
	"golang.org/x/sys/unix"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

func addClsact(link netlink.Link) error {
	qds, err := netlink.QdiscList(link)
	if err != nil {
		return fmt.Errorf("list qdisc for dev %s error, %w", link.Attrs().Name, err)
	}
	for _, q := range qds {
		if q.Type() == "clsact" {
			return nil
		}
	}

	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.HANDLE_CLSACT,
			Handle:    netlink.HANDLE_CLSACT & 0xffff0000,
		},
		QdiscType: "clsact",
	}
	if err := netlink.QdiscReplace(qdisc); err != nil {
		return fmt.Errorf("replace clsact qdisc for dev %s error, %w", link.Attrs().Name, err)
	}
	return nil
}

func createTCU32Filter(masterIndex int, ipnet *net.IPNet) *netlink.U32 {
	ip := ipnet.IP.Mask(ipnet.Mask).To4()
	mask := net.IP(ipnet.Mask).To4()

	return &netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: masterIndex,
			Priority:  40000,
			Protocol:  unix.ETH_P_IP,
		},
		Sel: &netlink.TcU32Sel{
			Nkeys: 1,
			Flags: nl.TC_U32_TERMINAL,
			Keys: []netlink.TcU32Key{
				{
					Mask: binary.BigEndian.Uint32(mask),
					Val:  binary.BigEndian.Uint32(ip),
					Off:  16,
				},
			},
		},
	}
}

func creatTCRedirectActions(dstIndex int) []netlink.Action {
	mirredAct := netlink.NewMirredAction(dstIndex)
	mirredAct.MirredAction = netlink.TCA_INGRESS_REDIR

	return []netlink.Action{mirredAct}
}

func filterMatch(u32f *netlink.U32, filter netlink.Filter) bool {
	tou32f, ok := filter.(*netlink.U32)
	if !ok {
		fmt.Printf("CompareRule, not u32 filter, ignore\n")
		return false
	}

	if u32f.Attrs().LinkIndex != tou32f.Attrs().LinkIndex || 
		u32f.Attrs().Protocol != tou32f.Attrs().Protocol ||
		len(u32f.Sel.Keys) != len(tou32f.Sel.Keys) {
		fmt.Printf("CompareRule, filter attr diffrent, ignore\n")
		return false
	}
	if u32f.Sel.Keys[0].Mask != tou32f.Sel.Keys[0].Mask ||
		u32f.Sel.Keys[0].Off != tou32f.Sel.Keys[0].Off ||
		u32f.Sel.Keys[0].Val != tou32f.Sel.Keys[0].Val {
		fmt.Printf("CompareRule, filter keys diffrent, ignore\n")
		return false
	}

	fmt.Printf("CompareRule, found same filter\n")
	return true
}

/*
tc qdisc add dev enp0s8 clsact
tc filter add dev enp0s8 egress proto ip u32 match ip dst 172.23.0.0/24 action tunnel_key unset pipe action mirred ingress redirect dev glue
tc filter show dev enp0s8 parent ffff:fff3
*/
func UpdateIpvlanTcConfig(conf GlueSubnetConf) error {
	fmt.Printf("UpdateIpvlanTcConfig: check clsact for master card %v\n", conf.Master.Master)

	link, _ := netlink.LinkByName(conf.Master.Master)
	linkto, _ := netlink.LinkByName(DefaltGlueDeviceName)

	// 增加clsact
	err := addClsact(link)
	if err != nil {
		return fmt.Errorf("ERROR: add clsact failed - %v\n", err)
	}

	// 增加Filter
	fmt.Printf("UpdateIpvlanTcConfig: add filter...\n")
	_, svcnet, _ := net.ParseCIDR(conf.ServiceCIDR)
	u32Filter := createTCU32Filter(link.Attrs().Index, svcnet)
	u32Filter.Actions = creatTCRedirectActions(linkto.Attrs().Index)

	parent := uint32(netlink.HANDLE_CLSACT&0xffff0000 | netlink.HANDLE_MIN_EGRESS&0x0000ffff)
	filters, err := netlink.FilterList(link, parent)
	if err != nil {
		return fmt.Errorf("list egress filter for %s error, %w", link.Attrs().Name, err)
	}

	for _, filter := range filters {
		if filterMatch(u32Filter, filter) {
			fmt.Printf("UpdateIpvlanTcConfig: found repeat filter, delete [%+v]\n", filter)
			netlink.FilterDel(filter)
		}
	}

	// tc filter show dev enp0s8 parent ffff:fff3
	u32Filter.Parent = parent
	if err := netlink.FilterAdd(u32Filter); err != nil {
		return fmt.Errorf("add filter for %s error, %w", link.Attrs().Name, err)
	}

	fmt.Printf("UpdateIpvlanTcConfig: add filter success\n")
	return nil
}

func CleanTcConfig() {
	//tc filter show dev enp0s8 parent ffff:fff3
	link, _ := netlink.LinkByName(subnetConf.Master.Master)

	filter := &netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent   : uint32(netlink.HANDLE_CLSACT&0xffff0000 | netlink.HANDLE_MIN_EGRESS&0x0000ffff),
		},
	}

	err := netlink.FilterDel(filter)
	if err != nil {
		fmt.Printf("Clean TC Config fail - %v\n", err)
	} else {
		fmt.Printf("Clean TC Config success\n")
	}
}
