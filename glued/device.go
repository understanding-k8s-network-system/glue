package main


import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/coreos/go-iptables/iptables"
)

const (
	DefaltGlueDeviceName = "glue"
	DefaultGluePOSTChainName = "GLUE-POSTROUTING"
	DefaultGluePREChainName = "GLUE-PREROUTING"
)


func Ipv4ToUint32(ip []byte) uint32 {
	return (uint32(ip[0]) << 24) | (uint32(ip[1]) << 16) | (uint32(ip[2]) << 8) | uint32(ip[3])
}

func Uint32ToIpv4(i uint32) net.IP {
	return net.IPv4(byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
}

func mapMacvlanMode(mode string) netlink.MacvlanMode {
	switch {
    case mode == "passthru":
        return netlink.MACVLAN_MODE_PASSTHRU
    case mode == "private":
        return netlink.MACVLAN_MODE_PRIVATE
    case mode == "vepa":
        return netlink.MACVLAN_MODE_VEPA
    }
    return netlink.MACVLAN_MODE_BRIDGE // bridge
}

func mapIPVlanMode(mode string) netlink.IPVlanMode {
	switch {
    case mode == "l3":
        return netlink.IPVLAN_MODE_L3
    case mode == "l3s":
        return netlink.IPVLAN_MODE_L3S
    }
    return netlink.IPVLAN_MODE_L2 // l2
}

func CleanDevices() error {
	link,err := netlink.LinkByName(DefaltGlueDeviceName)
	if (err != nil){
		fmt.Printf("CleanDevices: no glue device found, ignore\n")
		return nil
	}

	fmt.Printf("CleanDevices: delete glue device...\n")
	return netlink.LinkDel(link)
}

/*
函数返回值：
	rangStart: 本节点负责的地址范围
	rangeEnd: 本节点负责的地址范围
	nodeIP: 当前节点的IP地址
	ipvlanGW: 本节点上pod的网关地址
	ipvlanSvcGW: 服务网络网关
*/
func getNetInfo(subnet *GlueSubnetConf) (rangStart net.IP, rangeEnd net.IP, nodeIP net.IP, ipvlangw net.IP, ipvlansvcgw net.IP, err error) {
	/*
		每个Node对应一个网段，该网段的最后一个地址给Node节点使用，节点上的地址有差异
		- 最后一个Node的地址为x.x.x.254，其他Node的地址为x.x.x.255
	*/
	_, nodeIpv4Net, err := net.ParseCIDR(subnet.NodeCIDR)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("parse node CIDR failed")
	}
	_, podIpv4Net, _ := net.ParseCIDR(subnet.PodCIDR)

	/* 将pod地址段和node地址段转换成UINT32类型，方便计算 */
	nodeIpv4Int := Ipv4ToUint32(nodeIpv4Net.IP)
	nodeMaskInt := ^Ipv4ToUint32(nodeIpv4Net.Mask)
	
	podIpv4Int := Ipv4ToUint32(podIpv4Net.IP)
	podMaskInt := ^Ipv4ToUint32(podIpv4Net.Mask)

	/*
	计算本节点负责的地址范围：
	1. 第一个节点上的第一个地址 和 最后一个节点的最后一个地址 不可用，需要排除掉；
	2. 每个节点上的第一个有效地址留给节点用，pod从第2个地址开始分配；
	3. 整个pod网段的最后一个有效地址预留，可以考虑作为全网的默认网关 172.24.7.254
	4. 整个pod网段的倒数第二个有效地址预留，作为服务网络的默认网关：172.24.7.253
	*/
	var rangeStartOff uint32 = 1;
	if nodeIpv4Int==podIpv4Int {  
		rangeStartOff++;   // 第一个节点减掉一个地址
	}
	var rangeEndOff uint32 = nodeMaskInt;
	if nodeIpv4Int+nodeMaskInt==podIpv4Int+podMaskInt {
		if subnet.Master.Type == "macvlan" {
			rangeEndOff -= 1   // macvlan最后一个节点减掉一个地址
		} else {
			rangeEndOff -= 3   // ipvlan最后一个节点减掉三个地址
		}
	}
	ipvlanGW := Uint32ToIpv4(podIpv4Int+podMaskInt-1)
	ipvlanSvcGW := Uint32ToIpv4(podIpv4Int+podMaskInt-2)

	return 	Uint32ToIpv4(nodeIpv4Int+rangeStartOff), 
			Uint32ToIpv4(nodeIpv4Int+rangeEndOff), 
			Uint32ToIpv4(nodeIpv4Int+rangeStartOff-1), 
			ipvlanGW, 
			ipvlanSvcGW, 
			nil	
}

func AddDevice(conf GlueSubnetConf) error {
	link,err := netlink.LinkByName(conf.Master.Master)
	if (err != nil){
		return err
	}

	la := netlink.NewLinkAttrs()
	la.Name = DefaltGlueDeviceName
	la.ParentIndex = link.Attrs().Index

	if conf.Master.Type == "macvlan" {
		fmt.Printf("AddDevice: start add macvlan device...\n")
		link := &netlink.Macvlan{
			LinkAttrs: la,
			Mode: mapMacvlanMode(conf.Master.Mode),
		}

		if err:=netlink.LinkAdd(link); err!=nil {
			return err
		}

		// 设置父接口为混杂模式
		netlink.SetPromiscOn(link)
		fmt.Printf("AddDevice: add macvlan device success\n")
		return nil
	}

	if conf.Master.Type == "ipvlan" {
		fmt.Printf("AddDevice: start add ipvlan device...\n")
		link := &netlink.IPVlan{
			LinkAttrs: la,
			Mode: mapIPVlanMode(conf.Master.Mode),
			//Flag: netlink.IPVLAN_FLAG_VEPA, // 此功能依赖外部交换机支持haripin模式
		}

		if err:=netlink.LinkAdd(link); err!=nil {
			return err
		}

		fmt.Printf("AddDevice: add ipvlan device success\n")
		return nil
	}

	return fmt.Errorf("Unsupported device type")
}


/*
// 在 PREROUTING 链上增加规则
iptables -t nat -N GLUE-PREROUTING
iptables -t nat -I PREROUTING -j GLUE-PREROUTING
iptables -t nat -A GLUE-PREROUTING -s 172.24.0.0/24 -d 172.23.0.0/24 -i glue -j KUBE-MARK-MASQ
*/
 func UpdateIptables(conf GlueSubnetConf) error {
 	fmt.Printf("Update iptables...\n")
	ipt, err := iptables.New()
	if err!=nil {
		fmt.Printf("Get iptables handler fail - %v\n", err);
		return err
	}

	fmt.Printf("check iptables rules...\n")
	isExists, err := ipt.ChainExists("nat", DefaultGluePREChainName)
	if err==nil && isExists {
		fmt.Printf("iptables rules exist, skip...\n");
		return nil
	}

	err = ipt.NewChain("nat", DefaultGluePREChainName)
	if err!=nil {
		fmt.Printf("iptables create chain %v fail - %v\n", DefaultGluePREChainName, err);
		return err
	}

	err = ipt.Append("nat", DefaultGluePREChainName, 
						"-s", conf.NodeCIDR, 
						"-d", conf.ServiceCIDR, 
						"-i", DefaltGlueDeviceName, "-j", "KUBE-MARK-MASQ")
	if err!=nil {
		fmt.Printf("iptables append rule to chain %v fail - %v\n", DefaultGluePREChainName, err);
		return err
	}

	err = ipt.Insert("nat", "PREROUTING", 1, "-j", DefaultGluePREChainName)
	if err!=nil {
		fmt.Printf("iptables insert chain %v to PREROUTING fail\n", DefaultGluePREChainName);
		return err
	}

	fmt.Printf("iptables rules update success\n")
	return nil
}

/*
// 在 PREROUTING 链上删除规则
iptables -t nat -D PREROUTING -j GLUE-PREROUTING
iptables -t nat -F GLUE-PREROUTING
iptables -t nat -X GLUE-PREROUTING
*/
func CleanIptables() error {
 	fmt.Printf("Clean iptables...\n")
	ipt, err := iptables.New()
	if err!=nil {
		fmt.Printf("Get iptables handler fail - %v\n", err);
		return err
	}

	fmt.Printf("delete iptables rules...\n")
	ipt.Delete("nat", "PREROUTING", "-j", DefaultGluePREChainName)
	ipt.ClearChain("nat", DefaultGluePREChainName)
	ipt.DeleteChain("nat", DefaultGluePREChainName)

	return nil
}

func UpdateGlueDev(conf GlueSubnetConf) (error) {
	fmt.Printf("Update Glue Device..\n")
	if err := CleanDevices(); err != nil {
		return fmt.Errorf("ERROR: CleanDevices old config fail - %v\n", err)
	}

	err := AddDevice(conf)
	if err != nil {
		fmt.Printf("AddDevice: Add glue device failed, err = %v\n", err)
		return fmt.Errorf("ERROR: Add glue device fail, err=%+v\n", err)
	}
	
	glueDev, err := netlink.LinkByName(DefaltGlueDeviceName) 
	if err!=nil {
		fmt.Printf("AddDevice: Cannot get glue device err = %v\n", err)
		return err
	}

	_, _, nodeIP, _, _, err := getNetInfo(&conf)
	if err != nil {
		return err
	}

	// 配置本机地址，直接使用pod掩码
	_, myip, _ := net.ParseCIDR(conf.PodCIDR)
	myip.IP = nodeIP
	
	fmt.Printf("Set Glue Device addr as %s\n", myip.String())

	addr, _:= netlink.ParseAddr(myip.String())
	err = netlink.AddrAdd(glueDev, addr)
	if err != nil {
		fmt.Printf("AddDevice: Add addr failed, err = %v\n", err)
	}

	// 使能设备
	netlink.LinkSetUp(glueDev)

	// 更新iptables配置
	UpdateIptables(conf)

	// 更新ipvlan配置
	if conf.Master.Type == "ipvlan" {
		err = UpdateIpvlanTcConfig(conf)
		if err != nil{
			fmt.Printf("update ipvlan paras failed\n")
			return fmt.Errorf("update ipvlan paras failed\n")
		}
	}

	return nil
}

func GetDefaultGatewayInterface() (string, error) {
	routes, err := netlink.RouteList(nil, syscall.AF_INET)
	if err != nil {
		return "", err
	}

	for _, route := range routes {
		if route.Dst == nil || route.Dst.String() == "0.0.0.0/0" {
			if route.LinkIndex <= 0 {
				return "", errors.New("Found default route but could not determine interface")
			}
			intf, err := net.InterfaceByIndex(route.LinkIndex)
			if err != nil {
				return "", errors.New("Cannot get interface name")
			}

			return intf.Name, nil
		}
	}

	return "", errors.New("Unable to find default route")
}
