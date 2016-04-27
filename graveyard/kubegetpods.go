package main

import (
	"fmt"
	"k8s.io/kubernetes/pkg/api"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/client/restclient"
	"encoding/json"
)

const Host string = "http://10.114.209.50:8080"

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
		LabelSelector: 	labels.Everything(),
		FieldSelector:	fields.Everything(),
	}

	pods, err := client.Pods(api.NamespaceDefault).List(listopt)
	if err != nil {
		// handle error
	}

	jsonpods, err := json.MarshalIndent(pods, "", "     ")
		if err != nil {
		// handle error
	}

	fmt.Printf("%s\n", jsonpods)
	fmt.Printf("%T\n", jsonpods)

}
