package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"syscall"
	"path/filepath"

	"github.com/vishvananda/netlink"
	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ns"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

/*
NetConf 文件格式
{
    "cniVersion": "0.3.1",
    "name": "mynet",
	"type": "glue",
}

子网参数文件格式
{
    "podCIDR" : "172.24.0.0/21",
    "nodeCIDR" : "172.24.0.0/24",
	"Master" : {
		"type": "macvlan", 
    	"master": "enp0s8",
		"mode" : "bridge"
	}
}
*/

const (
	defaultSubnetFile = "/run/glue/subnet.json"
	defaultDataDir    = "/var/lib/cni/glue"
)

type NetConf struct {
	types.NetConf

	IPAM       map[string]interface{} `json:"ipam,omitempty"`
	Delegate   map[string]interface{} `json:"delegate"`
	SubnetFile    string              `json:"subnetFile"`
	DataDir       string              `json:"dataDir"`
}

// Glue 子网参数配置，由glue容器动态生成
type GlueSubnetConf struct {
	PodCIDR       string `json:"podCIDR"`
	ServiceCIDR   string `json:"serviceCIDR"`
	NodeCIDR      string `json:"nodeCIDR"`
	Master struct {
		Type   string `yaml:"type"`
		Master string `yaml:"master"`
		Mode   string `yaml:"mode"`
	}
	DefaultNeighMac string `yaml:"defaultNeighMac,omitempty"`
}

func hasKey(m map[string]interface{}, k string) bool {
	_, ok := m[k]
	return ok
}

func isString(i interface{}) bool {
	_, ok := i.(string)
	return ok
}

// 加载cni配置
func loadNetConf(bytes []byte) (*NetConf, error) {
	n := &NetConf{
		SubnetFile: defaultSubnetFile,
		DataDir:    defaultDataDir,
	}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}

	if n.Delegate == nil {
		n.Delegate = make(map[string]interface{})
	}

	n.Delegate["cniVersion"] = n.CNIVersion
	return n, nil
}

func loadGlueSubnet(path string) (*GlueSubnetConf, error) {
	netConfBytes, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Per spec should ignore error if resources are missing / already removed
			return nil, err
		}
		return nil, err
	}

	subnet := &GlueSubnetConf{}
	if err = json.Unmarshal(netConfBytes, subnet); err != nil {
		return nil, fmt.Errorf("failed to parse netconf: %v", err)
	}

	if subnet.PodCIDR == "" || subnet.NodeCIDR == "" {
		return nil, fmt.Errorf("get gule config fail, no PodCIDR/NodeCIDR found")
	}
	if subnet.Master.Type == "" {
		return nil, fmt.Errorf("invalid glue subnet file, no 'type' field found")
	}
	if subnet.Master.Master == "" {
		return nil, fmt.Errorf("invalid glue subnet file, no 'master' field found")
	}
	if subnet.DefaultNeighMac == "" {
		subnet.DefaultNeighMac = "08:60:83:00:00:00"
	}

	return subnet, nil
}

func Ipv4ToUint32(ip []byte) uint32 {
	return (uint32(ip[0]) << 24) | (uint32(ip[1]) << 16) | (uint32(ip[2]) << 8) | uint32(ip[3])
}

func Uint32ToIpv4(i uint32) net.IP {
	return net.IPv4(byte(i>>24), byte(i>>16), byte(i>>8), byte(i))
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

func genDelegateInfo(n *NetConf, subnet *GlueSubnetConf) (map[string]string, error) {
	n.Delegate["name"] = n.Name
	n.Delegate["type"] = subnet.Master.Type
	n.Delegate["master"] = subnet.Master.Master
	n.Delegate["cniVersion"] = n.CNIVersion

	if subnet.Master.Mode != "" {
		n.Delegate["mode"] = subnet.Master.Mode
	}

	start, end, nodeIP, ipvlanGW, ipvlanSvcGW, err := getNetInfo(subnet)
	if err != nil {
		return nil, err
	}

	// 生成IPAM参数
	ipam := map[string]interface{}{}
	ipam["type"] = "host-local"

	// 设置本节点负责的地址段
	var rangesSlice [][]map[string]interface{}
	if subnet.Master.Type == "macvlan" {
		rangesSlice = append(rangesSlice, []map[string]interface{}{
			{
				"subnet":     subnet.PodCIDR,
				"rangeStart": start.String(),
				"rangeEnd":   end.String(),
				"gateway":    nodeIP.String(),  // macvlan使用节点ip作为网关
			},
		})
	} else {
		rangesSlice = append(rangesSlice, []map[string]interface{}{
			{
				"subnet":     subnet.PodCIDR,
				"rangeStart": start.String(),
				"rangeEnd":   end.String(),
				"gateway":    ipvlanGW.String(),    // ipvlan用最后一个有效地址作为网关
			},
		})
	}

	ipam["ranges"] = rangesSlice

	rtes := []types.Route{}
	
	// 默认路由不指定网关地址，使用CNI配置文件中的网关
	rtes = append(rtes, types.Route{ Dst: net.IPNet { net.IPv4(0,0,0,0), net.CIDRMask(0,32)}})

	// 仅ipvlan场景需要增加服务网关
	if subnet.Master.Type == "ipvlan" {
		_, svcNet, _ := net.ParseCIDR(subnet.ServiceCIDR)
		rtes = append(rtes, types.Route{ Dst: *svcNet, GW: ipvlanSvcGW } ) 		
	}

	ipam["routes"] = rtes
	n.Delegate["ipam"] = ipam

	// 计算neighs
	nei := make(map[string]string)
	if subnet.Master.Type == "ipvlan" {
		nei[ipvlanSvcGW.String()] = subnet.DefaultNeighMac
	}

	return nei, nil
}


func saveContNetConf(containerID, dataDir string, netconf []byte) (string, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dataDir, containerID)
	return path, ioutil.WriteFile(path, netconf, 0600)
}


// NeighList: [{LinkIndex:2 Family:2 State:2 Type:1 Flags:0 IP:172.24.0.212 HardwareAddr:08:00:27:7b:78:5b LLIPAddr:<nil> Vlan:0 VNI:0 MasterIndex:0}]
func updateNeigh(neighs map[string]string, args *skel.CmdArgs) error {
	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	ifName := "eth0"
	for k, v := range neighs {
	    //fmt.Printf("Neigh key = %v, value = %v\n", k, v)

	    ip := net.ParseIP(k)
	    hw, err := net.ParseMAC(v)
	    if err != nil || ip == nil {
	    	//fmt.Printf("Parse Neigh fail, key = %v, value = %v\n", k, v)
	    	return fmt.Errorf("Parse neigh arg fail, key = %v, value = %v\n", k, v)
	    }

		err = netns.Do(func(_ ns.NetNS) error {
			iflink, err := netlink.LinkByName(ifName)
		    if err != nil {
				return fmt.Errorf("failed to get ipvlan device %q: %v", ifName, err)
			}

		    neigh := &netlink.Neigh {
				LinkIndex: iflink.Attrs().Index,
				Family: syscall.AF_INET,
				State: netlink.NUD_PERMANENT,
				IP: ip, 
				HardwareAddr: hw,
			}

			err = netlink.NeighAdd(neigh)
			if err != nil {
				//fmt.Printf("failed to add neigh key = %v, value = %v\n", k, v)
				return fmt.Errorf("failed to add neigh key = %v, value = %v\n", k, v)
			}
			return nil
		})

		if err != nil {
			return err
		}
	}
	return nil
}

func cmdAdd(args *skel.CmdArgs) error {
	// 配置数据来自标准输入
	n, err := loadNetConf(args.StdinData)
	if err != nil {
		return err
	}
	//fmt.Printf("Netconf info: %+v\n", n)

	subnet, err := loadGlueSubnet(n.SubnetFile)
	if err != nil {
		return err
	}

	neighs, err := genDelegateInfo(n, subnet)
	if err != nil {
		return fmt.Errorf("failed to generate delegate info: %w", err)
	}
	
	//fmt.Printf("n.Delegate = %+v\n", n.Delegate)
	//return fmt.Errorf("return")

	buf, _ := json.Marshal(n.Delegate)
	path, err := saveContNetConf(args.ContainerID, n.DataDir, buf)
	if err != nil {
		return err
	}

	result, err := invoke.DelegateAdd(context.TODO(), n.Delegate["type"].(string), buf, nil)
	if err != nil {
		_ = os.Remove(path)
		return err
	}

	// 更新neigh参数
	updateNeigh(neighs, args)
	return types.PrintResult(result, n.CNIVersion)
}

func consumeContNetConf(containerID, dataDir string) (func(error), []byte, error) {
	path := filepath.Join(dataDir, containerID)
	cleanup := func(err error) {
		if err == nil {
			// Ignore errors when removing - Per spec safe to continue during DEL
			_ = os.Remove(path)
		}
	}
	netConfBytes, err := ioutil.ReadFile(path)
	return cleanup, netConfBytes, err
}

func cmdDel(args *skel.CmdArgs) error {
	// 配置数据来自标准输入
	n, err := loadNetConf(args.StdinData)
	if err != nil {
		return err
	}

	cleanup, netConfBytes, err := consumeContNetConf(args.ContainerID, n.DataDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Per spec should ignore error if resources are missing / already removed
			return nil
		}
		return err
	}

	// cleanup will work when no error happens
	defer func() {
		cleanup(err)
	}()

	ncToDel := &types.NetConf{}
	if err = json.Unmarshal(netConfBytes, ncToDel); err != nil {
		return fmt.Errorf("failed to parse netconf: %v", err)
	}

	return invoke.DelegateDel(context.TODO(), ncToDel.Type, netConfBytes, nil)
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("glue"))
}

func cmdCheck(args *skel.CmdArgs) error {
	return nil
}
