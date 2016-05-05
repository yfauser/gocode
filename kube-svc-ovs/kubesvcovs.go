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
	"sync"
	"time"
)

var (
	Host    string = "http://10.114.209.77:8080"
	listopt        = api.ListOptions{LabelSelector: labels.Everything(), FieldSelector: fields.Everything()}
	Info    *log.Logger
	Error   *log.Logger
)

var mutex = &sync.Mutex{}

type K8ssvc struct {
	Name         string
	Namespace    string
	Uid          types.UID
	ClusterIP    string
	Ports        []api.ServicePort
	Endpoints    []api.EndpointSubset
	EndpointsUid types.UID
	OvsGroup     int
	Deleted      bool
}

func formatK8svcJson(in *api.Service) string {
	jsonsvcs, err := json.MarshalIndent(in, "", "     ")
	if err != nil {
		Error.Printf("Could not parse Object and return JSON", err)
	}
	return string(jsonsvcs)
}

func formatK8endpointJson(in *api.Endpoints) string {
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

func constructCtEntries(service K8ssvc) (ct string) {
	var entry string = ""
	CtPrefix := "group_id=" + strconv.Itoa(service.OvsGroup) + ",type=select,"
	if len(service.Endpoints) == 0 {
		ct = CtPrefix
		return ct
	}
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

func repOvsGroup(service K8ssvc) {
	// Delete and re-add group from OVS (The better solution is insert-buckets - TODO)
	delSvcOvs(service)
	// add a new Service Group to OVS
	addOvsSvc(service)
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
		ovsGroupId++
		svcToAdd := constructService(client, &item)
		addServiceToArray(client, 0, &svcToAdd, &services, ovsGroupId)
	}
	return services
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
	service.EndpointsUid = endpoints.UID
	service.Deleted = false
	return service
}

func addServiceToArray(client *client.Client, replaceIndex int, newService *K8ssvc, services *[]K8ssvc, ovsGroupId int) {
	// Adds new services to internal Services Slice
	if replaceIndex == 0 {
		newService.OvsGroup = ovsGroupId
		*services = append(*services, *newService)
	} else {
		newService.OvsGroup = ovsGroupId
		(*services)[replaceIndex] = *newService
	}
}

func addrInSubsets(currentSvc []api.EndpointSubset, addr api.EndpointAddress) bool {
	for _, sub := range currentSvc {
		for _, addrItem := range sub.Addresses {
			if addr.TargetRef.UID == addrItem.TargetRef.UID {
				return true
			}
		}
	}
	return false
}

func endpointDiff(modEndpoints api.Endpoints, svcEndpoints []api.EndpointSubset) (added []api.EndpointAddress, removed []api.EndpointAddress) {
	// If the modified services has more Addresses than the existing, add them
	for _, sub := range modEndpoints.Subsets {
		for _, addr := range sub.Addresses {
			if !addrInSubsets(svcEndpoints, addr) {
				added = append(added, addr)
			}
		}
	}
	// if the existing service has more Addresses than the modified, delete them
	for _, sub := range svcEndpoints {
		for _, addr := range sub.Addresses {
			if !addrInSubsets(modEndpoints.Subsets, addr) {
				removed = append(removed, addr)
			}
		}
	}
	return added, removed
}

func modifyEndpoints(endpointsObj *api.Endpoints, services *[]K8ssvc) {
	for index, service := range *services {
		if service.EndpointsUid == endpointsObj.UID {
			Info.Printf("Endpoints for Service %s modified", endpointsObj.Name)
			addedAddr, remAddr := endpointDiff(*endpointsObj, service.Endpoints)
			Info.Printf("Added Addresses: %s , Removed Adresses: %s\n", addedAddr, remAddr)
			(*services)[index].Endpoints = endpointsObj.Subsets
			repOvsGroup((*services)[index])
		}
	}
}

func addSvc(client *client.Client, eventObject runtime.Object, services *[]K8ssvc) bool {
	// called if service gets added in K8s
	mutex.Lock()
	defer mutex.Unlock()
	addedService := constructService(client, eventObject.(*api.Service))
	ovsGroupId := 0
	svcIndex := 0
	Info.Printf("New Service %s Added\n", addedService.Name)
	for index, service := range *services {
		if service.Uid == addedService.Uid {
			Info.Printf("Service %s is already existing, skipping", addedService.Name)
			return false
		} else if service.Deleted {
			svcIndex = index
			ovsGroupId = service.OvsGroup
		}
	}
	if svcIndex == 0 {
		ovsGroupId = len(*services) + 100
	}
	addServiceToArray(client, svcIndex, &addedService, services, ovsGroupId)
	addOvsSvc(addedService)
	return true
}

func modSvc(client *client.Client, eventObject runtime.Object, services *[]K8ssvc) {
	// called if service gets added in K8s
	// nothing implemented yet
	Info.Printf("Service %s Modified\n", eventObject)
}

func delSvc(client *client.Client, eventObject runtime.Object, services *[]K8ssvc) {
	// called if service gets added in K8s
	mutex.Lock()
	defer mutex.Unlock()
	deletedService := constructService(client, eventObject.(*api.Service))
	Info.Printf("Service %s Deleted\n", deletedService.Name)
	for index, service := range *services {
		if service.Uid == deletedService.Uid {
			if service.Deleted {
				Info.Printf("Service %s is already Deleted, skipping", deletedService.Name)
			} else {
				Info.Printf("Deleting service: %s", service.Name)
				(*services)[index].Deleted = true
				delSvcOvs(service)
			}
		}
	}
}

func addEndpoints(client *client.Client, eventObject runtime.Object, services *[]K8ssvc) {
	// called if full Endpoint list is Added
	// not implemented yet - not sure when this is called
	Info.Printf("Endpoints ADDED")
}

func modEndpoints(client *client.Client, eventObject runtime.Object, services *[]K8ssvc) {
	// called when Endpoints are modified, e.g. when Pods with the right tags are started
	// not implemented yet - not sure when this is called
	Info.Printf("Endpoint MODIFIED")
	modifyEndpoints(eventObject.(*api.Endpoints), services)
}

func delEndpoints(client *client.Client, eventObject runtime.Object, services *[]K8ssvc) {
	// called when full Endpoint List is Deleted
	Info.Printf("Endpoint DELETED")
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

	watchSvcObj, err := client.Services(api.NamespaceDefault).Watch(listopt)
	if err != nil {
		Error.Printf("could not set a watch on service Objects with the K8s API")
	}
	watchSvcChan := watchSvcObj.ResultChan()

	watchEndpointObj, err := client.Endpoints(api.NamespaceDefault).Watch(listopt)
	if err != nil {
		Error.Printf("could not set a watch on Endpoints Objects with the K8s API")
	}
	watchEndpointChan := watchEndpointObj.ResultChan()

	for {
		select {
		case endpointsWatch := <-watchEndpointChan:
			Info.Printf("receive on channel is %s, type %s %T \n", endpointsWatch, endpointsWatch.Type, endpointsWatch.Type)
			Info.Printf("Received Endpoints Details: \n%s\n", formatK8endpointJson(endpointsWatch.Object.(*api.Endpoints)))
			switch endpointsWatch.Type {
			case "ADDED":
				addEndpoints(client, endpointsWatch.Object, &svcs)
			case "DELETED":
				delEndpoints(client, endpointsWatch.Object, &svcs)
			case "MODIFIED":
				modEndpoints(client, endpointsWatch.Object, &svcs)
			}
		case serviceWatch := <-watchSvcChan:
			Info.Printf("receive on channel is %s, type %s %T \n", serviceWatch, serviceWatch.Type, serviceWatch.Type)
			Info.Printf("Received Service Details: \n%s\n", formatK8svcJson(serviceWatch.Object.(*api.Service)))
			switch serviceWatch.Type {
			case "ADDED":
				time.Sleep(500 * time.Millisecond)
				addSvc(client, serviceWatch.Object, &svcs)
			case "DELETED":
				time.Sleep(500 * time.Millisecond)
				delSvc(client, serviceWatch.Object, &svcs)
			case "MODIFIED":
				time.Sleep(500 * time.Millisecond)
				delSvc(client, serviceWatch.Object, &svcs)
			}
		}
	}

}
