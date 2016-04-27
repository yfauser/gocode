package main

import (
	"encoding/json"
	"fmt"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/restclient"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
)

const Host string = "http://10.114.209.68:8080"

//func svcsWatcher(c *client.Client, o api.ListOptions) *watch.Interface {
//	watchSvcs, err := c.Services(api.NamespaceDefault).Watch(o)
//	if err != nil {
// handle error
//	}
//	return &watchSvcs

func main() {

	config := &restclient.Config{
		Host:     Host,
		Insecure: true,
	}

	client, err := client.New(config)
	if err != nil {
		// handle error
	}

	listopt := api.ListOptions{
		LabelSelector: labels.Everything(),
		FieldSelector: fields.Everything(),
	}

	svcs, err := client.Services(api.NamespaceDefault).List(listopt)
	if err != nil {
		// handle error
	}

	jsonsvcs, err := json.MarshalIndent(svcs, "", "     ")
	if err != nil {
		// handle error
	}

	fmt.Printf("%s\n", jsonsvcs)

	watchObj, err := client.Services(api.NamespaceDefault).Watch(listopt)
	if err != nil {
		// handle error
	}

	watchChan := watchObj.ResultChan()

	fmt.Printf("%T\n", watchObj)
	fmt.Printf("%T\n", watchChan)

	for {
		receive := <-watchChan

		jsonsvcs, err := json.MarshalIndent(receive.Object, "", "     ")
		if err != nil {
			// handle error
		}
		fmt.Printf("%s\n", receive)
		fmt.Printf("%s\n", jsonsvcs)

	}

}
