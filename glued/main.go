package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"path/filepath"

	"encoding/json"
	"io/ioutil"

	yaml "gopkg.in/yaml.v2"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	"github.com/containernetworking/plugins/pkg/ip"
)

const (
	defaultSubnetFile = "/run/glue/subnet.json"
)

type CMClusterConfig struct {
	Networking struct {
		PodSubnet     string `yaml:"podSubnet"`
		ServiceSubnet string `yaml:"serviceSubnet"`
	}
}

// Glue 子网参数配置，由glue容器动态生成
type GlueSubnetConf struct {
	PodCIDR     string `json:"podCIDR"`
	ServiceCIDR string `json:"serviceCIDR"`
	NodeCIDR    string `json:"nodeCIDR"`
	Master      struct {
		Type   string `yaml:"type"`
		Master string `yaml:"master"`
		Mode   string `yaml:"mode"`
	}
	DefaultNeighMac string `yaml:"defaultNeighMac,omitempty"`
}

/*
GlueSubnetConf 配置文件范例：
{
    "podCIDR" : "172.24.0.0/21",
    "nodeCIDR" : "172.24.0.0/24",
	"master" : {
		"type": "macvlan",
    	"master": "enp0s8",
		"mode" : "bridge"
	}
}

{
    "podCIDR" : "172.24.0.0/21",
    "serviceCIDR" : "172.23.0.0/24",
    "nodeCIDR" : "172.24.6.0/24",
	"master" : {
		"type": "ipvlan",
		"master": "enp0s8",
		"mode" : "l2"
	},
	"defaultNeighMac":"00:11:11:22:22:33"
}
*/

func showGlueRunning(g *GlueSubnetConf) {
	fmt.Printf("Glue running info: \n")
	fmt.Printf("    kubeconfig file     : %s\n", *argKubeconfig)
	fmt.Printf("    glue subnet file    : %s\n", *argSubnetFile)
	fmt.Printf("    pod CIDR            : %s\n", g.PodCIDR)
	fmt.Printf("    service CIDR        : %s\n", g.ServiceCIDR)
	fmt.Printf("    node CIDR           : %s\n", g.NodeCIDR)
	fmt.Printf("    stick to CNI plugin :\n")
	fmt.Printf("        type   = %v\n", g.Master.Type)
	fmt.Printf("        master = %v\n", g.Master.Master)
	fmt.Printf("        mode   = %v\n", g.Master.Mode)
	fmt.Printf("    ipvlan default neigh mac : %s\n", g.DefaultNeighMac)
}

var (
	argKubeconfig     *string
	argPodCIDR        *string
	argServiceCIDR    *string
	argNodeCIDR       *string
	argSubnetFile     *string
	argStickCniType   *string
	argStickCniMaster *string
	argStickCniMode   *string
	argIpvlanNeighMac *string

	subnetConf GlueSubnetConf
)

func StringInArr(arr []string, toFind string) bool {
	for _, s := range arr {
		if s == toFind {
			return true
		}
	}
	return false
}

func FileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	//isnotexist来判断，是不是不存在的错误
	if os.IsNotExist(err) { //如果返回的错误类型使用os.isNotExist()判断为true，说明文件或者文件夹不存在
		return false, nil
	}
	return false, err //如果有错误了，但是不是不存在的错误，所以把这个错误原封不动的返回
}

func getClientSet() (*kubernetes.Clientset, error) {
	fmt.Printf("Try to get client set.\n")
	// in-cluster
	saok, err := FileExists("/var/run/secrets/kubernetes.io/serviceaccount")
	if err == nil && saok {
		fmt.Printf("Found k8s serviceaccount, try...\n")
		config, err := rest.InClusterConfig()
		if err == nil {
			clientset, err := kubernetes.NewForConfig(config)
			if err == nil {
				fmt.Printf("Get InClusterConfig success, use serviceaccount.\n")
				return clientset, nil
			}
		}
		fmt.Printf("Get InClusterConfig fail, try kubeconfig\n")
	}

	fmt.Printf("use kubeconfig\n")
	// 如果输入了kubeconfig参数，就使用用户指定的，否则就用home目录的
	if *argKubeconfig != "" {
		if exist, err := FileExists(*argKubeconfig); err != nil || !exist {
			return nil, fmt.Errorf("Cannot access kubeconfig file %v\n", *argKubeconfig)
		}
	}

	// 加载kubeconfig配置文件，因此第一个参数为空字符串
	config, err := clientcmd.BuildConfigFromFlags("", *argKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("Cannot create client-go config\n")
	}

	fmt.Printf("Get clientset with kubeconfig success\n")
	// 实例化clientset对象
	return kubernetes.NewForConfig(config)
}

// 获取cluster配置
func getClusterCIDR(clientset *kubernetes.Clientset) (*CMClusterConfig, error) {
	conf := &CMClusterConfig{}

	// kubectl get cm -n kube-system kubeadm-config
	cm, err := clientset.CoreV1().ConfigMaps("kube-system").Get(context.TODO(), "kubeadm-config", metav1.GetOptions{})
	if err == nil {
		err = yaml.Unmarshal([]byte(cm.Data["ClusterConfiguration"]), &conf)
		//fmt.Printf("Kubeadm ClusterConfiguration: %+v\n", conf)
		return conf, nil
	}

	return nil, fmt.Errorf("ERROR: no Kubeadm ClusterConfiguration found\n")
}

func parseArg() error {
	//fmt.Printf("ARG: %+v\n", flag)
	argKubeconfig = flag.String("kubeconfig-file", "", "(optional) absolute path to the kubeconfig file")
	argSubnetFile = flag.String("subnet-file", defaultSubnetFile, "subnet file, default is "+defaultSubnetFile)
	argPodCIDR = flag.String("pod-cidr", "", "cluster podCIDR, if not set, use kubeadm-config")
	argServiceCIDR = flag.String("service-cidr", "", "cluster serviceCIDR")
	argNodeCIDR = flag.String("node-cidr", "", "node CIDR")

	argStickCniType = flag.String("stick-cni-type", "macvlan", "Stick to CNI Plugin, support macvlan/ipvlan, default is macvlan")
	argStickCniMaster = flag.String("stick-cni-master", "", "Stick to CNI Plugin, master netcard")
	argStickCniMode = flag.String("stick-cni-mode", "bridge", "Stick to CNI Plugin, work mode")

	argIpvlanNeighMac = flag.String("ipvlan-neigh-mac", "", "default neigh mac address for ipvlan")

	flag.Parse()

	// 检查参数
	if *argPodCIDR != "" && (*argServiceCIDR == "" || *argNodeCIDR == "") {
		return fmt.Errorf("ERROR: user-defined pod network, you must specify ServiceCIDR and NodeCIDR.\n")
	}
	if *argKubeconfig == "" {
		*argKubeconfig = filepath.Join(homedir.HomeDir(), ".kube", "config")
	}
	if *argStickCniType != "macvlan" && *argStickCniType != "ipvlan" {
		return fmt.Errorf("ERROR: Only support macvlan/ipvlan, use 'stick-cni-type'\n")
	}
	if *argStickCniType == "macvlan" && !StringInArr([]string{"bridge", "vepa", "passthru", "private"}, *argStickCniMode) {
		return fmt.Errorf("ERROR: macvlan mode %s not supported, only support bridge, vepa, passthru and private\n", *argStickCniMode)
	}
	if *argStickCniType == "ipvlan" && !StringInArr([]string{"l2", "l3", "l3s"}, *argStickCniMode) {
		return fmt.Errorf("ERROR: ipvlan mode %s not supported, only support l2, l3 or l3s\n", *argStickCniMode)
	}

	if *argStickCniMaster == "" {
		master, err := GetDefaultGatewayInterface()
		if err != nil {
			return fmt.Errorf("ERROR: Must specify Master netcard, use 'stick-cni-master'\n")
		}
		argStickCniMaster = &master
		fmt.Printf("Use default interface %v as master netcard\n", *argStickCniMaster)
	}

	if *argIpvlanNeighMac != "" {
		_, err := net.ParseMAC(*argIpvlanNeighMac)
		if err != nil {
			return fmt.Errorf("ERROR: parse mac address fail, please check 'ipvlan-neigh-mac'\n")
		}
	}

	// 保存参数
	subnetConf.Master.Type = *argStickCniType
	subnetConf.Master.Master = *argStickCniMaster
	subnetConf.Master.Mode = *argStickCniMode
	subnetConf.DefaultNeighMac = *argIpvlanNeighMac
	return nil
}

func writeSubnetConf() error {
	fmt.Printf("update subnet file : %v\n", *argSubnetFile)

	// 处理文件
	subNetfileDir := filepath.Dir(*argSubnetFile)
	if exist, err := FileExists(subNetfileDir); err != nil || !exist {
		if err := os.MkdirAll(subNetfileDir, 0700); err != nil {
			return fmt.Errorf("Error: create subnet dir [%s] fail\n", subNetfileDir)
		}
	}

	buf, _ := json.Marshal(subnetConf)
	return ioutil.WriteFile(*argSubnetFile, buf, 0600)
}

func UpdateGlueConf() {
	UpdateGlueDev(subnetConf)
	writeSubnetConf()
}

func MainLoop() {
	fmt.Printf("Enter main loop\n")
	counter := 1
	for {
		time.Sleep(10 * time.Second)
		//fmt.Printf("Counter = %d\n", counter)
		counter++
	}
}

func sysconfig() error {
	err := ip.EnableIP4Forward()
	if err != nil {
		return fmt.Errorf("Could not enable IPv4 forwarding: %v", err)
	}
	return nil
}

func tearDown(s os.Signal) {
	switch s {
	case syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT:
		fmt.Printf("\n\nProgram Exit, clean resources...\n", s)
		CleanDevices()
		CleanIptables()
		CleanTcConfig()

		// 清理待删除的文件
		fmt.Printf("delete subnet file %v\n", *argSubnetFile)
		os.Remove(*argSubnetFile)

		env := os.Getenv("GLUE_FILES_TO_COPY_ON_BOOT")
		//fmt.Printf("env GLUE_FILES_TO_COPY_ON_BOOT = %v\n", env)
		if env != "" {
			fmt.Printf("Delete file on delete...\n")
			filesToRCopy := strings.Split(env, ",")
			for _, pair := range filesToRCopy {
				kv := strings.Split(pair, ":")
				if len(kv) != 2 {
					fmt.Printf("Invalid args in env GLUE_FILES_TO_COPY_ON_BOOT, arg = %s, skip\n", filesToRCopy)
					continue
				}

				fmt.Printf("delete file %v\n", kv[1])
				err := os.Remove(kv[1])
				if err != nil {
					fmt.Printf("ERROR: %v\n", err)
				}
			}
		}

		os.Exit(0)
	default:
		fmt.Println("other signal", s)
	}
}

func copyFile(src string, dst string) error {
	fmt.Printf("copy %v to %v\n", src, dst)

	input, err := ioutil.ReadFile(src)
	if err != nil {
		fmt.Println(err)
		return fmt.Errorf("Read file %v err - %v", src, err)
	}

	err = ioutil.WriteFile(dst, input, 0755)
	if err != nil {
		return fmt.Errorf("creat file %v err - %v", dst, err)
	}
	return nil
}

func main() {
	fmt.Printf("Glue v0.1\n")

	//监听指定信号 ctrl+c kill
	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		for s := range c {
			tearDown(s)
		}
	}()

	// 根据环境变量取值决定是否拷贝文件
	env := os.Getenv("GLUE_FILES_TO_COPY_ON_BOOT")
	//fmt.Printf("env GLUE_FILES_TO_COPY_ON_BOOT = %v\n", env)
	if env != "" {
		fmt.Printf("Copy file on boot...\n")
		filesToRCopy := strings.Split(env, ",")
		for _, pair := range filesToRCopy {
			kv := strings.Split(pair, ":")
			if len(kv) != 2 {
				fmt.Printf("Invalid args in env GLUE_FILES_TO_COPY_ON_BOOT, arg = %s, skip\n", filesToRCopy)
				continue
			}

			os.Remove(kv[1])
			err := copyFile(kv[0], kv[1])
			if err != nil {
				fmt.Printf("ERROR: %v\n", err)
			}
		}
	}

	// 基础配置
	if err := sysconfig(); err != nil {
		fmt.Printf("%vl\n", err)
		return
	}

	// 参数解析
	if err := parseArg(); err != nil {
		fmt.Printf("%vl\n", err)
		return
	}

	// 获取cluster配置，优先使用用户参数中指定的网络配置
	fmt.Printf("Parse podCIDR\n")
	subnetConf.PodCIDR = *argPodCIDR
	subnetConf.ServiceCIDR = *argServiceCIDR
	subnetConf.NodeCIDR = *argNodeCIDR

	if subnetConf.PodCIDR != "" {
		fmt.Printf("Use user defined CIDR:\n")
		fmt.Printf("    podCIDR is %v\n", subnetConf.PodCIDR)
		fmt.Printf("    serviceCIDR is %v\n", subnetConf.ServiceCIDR)
		fmt.Printf("    nodeCIDR is %v\n", subnetConf.NodeCIDR)

		showGlueRunning(&subnetConf)
		UpdateGlueConf()
	} else {
		// 获取 clientset
		clientset, err := getClientSet()
		if err != nil {
			fmt.Printf("Error: getClientSet fail\n")
			return
		}

		conf, err := getClusterCIDR(clientset)
		if err != nil {
			fmt.Printf("Error: getKubeAdminConfig fail\n")
			return
		}

		subnetConf.PodCIDR = conf.Networking.PodSubnet
		subnetConf.ServiceCIDR = conf.Networking.ServiceSubnet

		fmt.Printf("Use Kubernetes configed CIDR:\n")
		fmt.Printf("    podCIDR is %v\n", subnetConf.PodCIDR)
		fmt.Printf("    serviceCIDR is %v\n", subnetConf.ServiceCIDR)

		// watch node配置
		fmt.Printf("Now watch nodes...\n")
		nodeWatcher, err := clientset.CoreV1().Nodes().Watch(context.TODO(), metav1.ListOptions{})
		if err != nil {
			fmt.Printf("Error: Get pod list fail!!!\n")
			return
		}

		go func() {
			for event := range nodeWatcher.ResultChan() {
				p, ok := event.Object.(*apiv1.Node)
				if !ok {
					fmt.Printf("unexpected type\n")
				}
				//fmt.Printf("Node Name is %+v\n", p.ObjectMeta.Name)
				hn, _ := os.Hostname()
				if p.ObjectMeta.Name != hn {
					continue
				}

				fmt.Printf("Node %+v changed\n", hn)
				if subnetConf.NodeCIDR == p.Spec.PodCIDR {
					continue
				}

				// POD CIDR有更新
				subnetConf.NodeCIDR = p.Spec.PodCIDR
				showGlueRunning(&subnetConf)
				UpdateGlueConf()
			}
		}()
	}

	MainLoop()
	return
}
