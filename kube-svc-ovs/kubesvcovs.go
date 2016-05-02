package main

import (
	"encoding/json"
	"io"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/restclient"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/types"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

var (
	Host    string = "http://10.114.209.77:8080"
	listopt        = api.ListOptions{LabelSelector: labels.Everything(), FieldSelector: fields.Everything()}
	Info    *log.Logger
	Error   *log.Logger
)

type K8ssvc struct {
	Name      string
	Namespace string
	Uid       types.UID
	ClusterIP string
	Ports     []api.ServicePort
	Endpoints []api.EndpointSubset
	OvsGroup  int
	Deleted   bool
}

func formatK8svcJson(in K8ssvc) string {
	jsonsvcs, err := json.MarshalIndent(in, "", "     ")
	if err != nil {
		Error.Printf("Could not parse Object and return JSON", err)
	}
	return string(jsonsvcs)
}

func openlog(filename string) *os.File {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalln("Failed to open log file", filename, ":", err)
	}
	return file
}

func createapiclient(config *restclient.Config) *client.Client {
	client, err := client.New(config)
	if err != nil {
		Error.Printf("Error connecting to the K8s API")
	}
	return client
}

func cmdexecutor(cmd *exec.Cmd, quite bool) string {
	Info.Printf("Executing command %s\n", strings.Join(cmd.Args, " "))
	output, err := cmd.CombinedOutput()
	if err != nil {
		Error.Printf("command %s returned failure and following stdout: %s\n", strings.Join(cmd.Args, " "), output)
		return string(output)
	}
	if len(output) > 0 && quite {
		Info.Printf("Command '%s' returned: %s\n", strings.Join(cmd.Args, " "), output)
	}
	return string(output)
}

func getallsvc(client *client.Client) []K8ssvc {
	// retrieve Service List for all Services
	Info.Printf("retrieving complete service list from K8s master")
	svcs, err := client.Services(api.NamespaceDefault).List(listopt)
	if err != nil {
		Error.Printf("could not get a service Object from the K8s API")
	}
	// retrieve all services and populate service struct
	ovsGroupId := 99
	services := []K8ssvc{}
	for _, item := range svcs.Items {
		var service K8ssvc
		ovsGroupId++
		service.Name = item.Name
		service.Namespace = item.Namespace
		service.Uid = item.UID
		service.Ports = item.Spec.Ports
		service.ClusterIP = item.Spec.ClusterIP
		endpoints, _ := client.Endpoints(item.Namespace).Get(item.Name)
		service.Endpoints = endpoints.Subsets
		service.OvsGroup = ovsGroupId
		service.Deleted = false
		services = append(services, service)
	}
	return services
}

func constructCtEntries(service K8ssvc) (ct string) {
	var entry string = ""
	CtPrefix := "group_id=" + strconv.Itoa(service.OvsGroup) + ",type=select,"
	for _, endpoint := range service.Endpoints {
		for _, ip := range endpoint.Addresses {
			entryPrefix := "bucket=weight=100,ct(nat(dst="
			entry = entry + entryPrefix + ip.IP + "),commit,table=2),"
		}
	}
	ct = CtPrefix + entry[:len(entry)-1]
	return ct
}

func addOvsSvc(service K8ssvc) {
	// add a new Service Group to OVS
	ctString := constructCtEntries(service)
	cmdexecutor(exec.Command("ovs-ofctl", "-O", "OpenFlow13", "add-group", "br0", ctString), true)
	for _, port := range service.Ports {
		var protocol string
		if port.Protocol == "TCP" {
			protocol = ",ip_proto=6,tcp_dst=" + strconv.Itoa(int(port.Port))
		}
		if port.Protocol == "UDP" {
			protocol = ",ip_proto=17,udp_dst=" + strconv.Itoa(int(port.Port))
		}
		cmdexecutor(exec.Command("ovs-ofctl", "-O", "OpenFlow13", "add-flow", "br0",
			"table=1,priority=100,ip,nw_dst="+service.ClusterIP+protocol+
				",actions=mod_tp_dst:"+strconv.Itoa(int(port.Port))+",group:"+
				strconv.Itoa(int(service.OvsGroup))), true)
	}
}

func delSvcOvs(service K8ssvc) {
	// Delete Service from OVS
	flowdelstring := "table=1, ip, nw_dst=" + service.ClusterIP
	groupdelstring := "group_id:" + strconv.Itoa(int(service.OvsGroup))
	cmdexecutor(exec.Command("ovs-ofctl", "-O", "OpenFlow13", "del-flows", "br0", flowdelstring), true)
	cmdexecutor(exec.Command("ovs-ofctl", "-O", "OpenFlow13", "del-groups", "br0", groupdelstring), true)
}

func addNatOVScatch() bool {
	// adds the catch flow rule for CT Nat'ed established connections
	cmdexecutor(exec.Command("ovs-ofctl", "-O", "OpenFlow13", "del-flows", "br0", "table=1,ip"), true)
	result := cmdexecutor(exec.Command("ovs-ofctl", "-O", "OpenFlow13", "add-flow", "br0",
		"table=1,priority=90,ip,action=ct(table=2,nat)"), true)
	if result == "" {
		return true
	}
	return false
}

func createInitialSvc(services []K8ssvc) {
	// Create all services that were retrieved on Init
	Info.Printf("Creating initial set of services in OVS")
	if !addNatOVScatch() {
		Error.Printf("Failed to add initial flow entries, is Kubelet with the OVS Plugin started?")
	}
	for _, service := range services {
		addOvsSvc(service)
	}
}

func constructService(client *client.Client, svcObject *api.Service) K8ssvc {
	var service K8ssvc
	service.Name = svcObject.Name
	service.Namespace = svcObject.Namespace
	service.Uid = svcObject.UID
	service.Ports = svcObject.Spec.Ports
	service.ClusterIP = svcObject.Spec.ClusterIP
	endpoints, _ := client.Endpoints(svcObject.Namespace).Get(svcObject.Name)
	service.Endpoints = endpoints.Subsets
	service.Deleted = false
	return service
}

func addSvc(client *client.Client, eventObject runtime.Object, services *[]K8ssvc) bool {
	// called if service gets added in K8s
	addedService := constructService(client, eventObject.(*api.Service))
	Info.Printf("New Service %s Added\n", addedService.Name)
	for _, service := range *services {
		if service.Uid == addedService.Uid {
			Info.Printf("Service %s is already existing, skipping", addedService.Name)
			return false
		}
	}
	return true
}

func modSvc(client *client.Client, eventObject runtime.Object, services *[]K8ssvc) {
	// called if service gets added in K8s
	Info.Printf("Service %s Modified\n", eventObject)
}

func delSvc(client *client.Client, eventObject runtime.Object, services *[]K8ssvc) {
	// called if service gets added in K8s
	deletedService := constructService(client, eventObject.(*api.Service))
	Info.Printf("Service %s Deleted\n", deletedService.Name)
	for _, service := range *services {
		if service.Uid == deletedService.Uid {
			Info.Printf("Deleting service: %s", service.Name)
			service.Deleted = true
			delSvcOvs(service)
		}
	}
}

func main() {
	if len(os.Args) > 1 {
		Host = os.Args[1]
	}

	logfile := openlog("/tmp/watcher.log")
	defer logfile.Close()
	multi := io.MultiWriter(logfile, os.Stdout)
	Info = log.New(multi, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	Error = log.New(multi, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)

	config := &restclient.Config{Host: Host, Insecure: true}

	client := createapiclient(config)
	svcs := getallsvc(client)
	createInitialSvc(svcs)

	watchObj, err := client.Services(api.NamespaceDefault).Watch(listopt)
	if err != nil {
		Error.Printf("could not set a watch on service Objects with the K8s API")
	}
	watchChan := watchObj.ResultChan()

	for {
		receive := <-watchChan
		switch receive.Type {
		case "ADDED":
			addSvc(client, receive.Object, &svcs)
		case "MODIFIED":
			modSvc(client, receive.Object, &svcs)
		case "DELETED":
			delSvc(client, receive.Object, &svcs)
		}
	}

}
