/*
Copyright 2014 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// kube2sky is a bridge between Kubernetes and SkyDNS.  It watches the
// Kubernetes master for changes in Services and manifests them into etcd for
// SkyDNS to serve as DNS records.

// openxxs: add hostname of pod and node into DNS record
package main

import (
	"encoding/json"
	goflag "flag"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	etcd "github.com/coreos/etcd/client"
	"github.com/golang/glog"
	skymsg "github.com/skynetservices/skydns/msg"
	flag "github.com/spf13/pflag"
	"golang.org/x/net/context"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/endpoints"
	"k8s.io/kubernetes/pkg/api/unversioned"
	kcache "k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/client/restclient"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	kclientcmd "k8s.io/kubernetes/pkg/client/unversioned/clientcmd"
	kframework "k8s.io/kubernetes/pkg/controller/framework"
	kselector "k8s.io/kubernetes/pkg/fields"
	etcdstorage "k8s.io/kubernetes/pkg/storage/etcd"
	"k8s.io/kubernetes/pkg/util/validation"
	"k8s.io/kubernetes/pkg/util/wait"
)

var (
	// TODO: switch to pflag and make - and _ equivalent.
	argDomain              = flag.String("domain", "cluster.local", "domain under which to create names")
	argEtcdMutationTimeout = flag.Duration("etcd_mutation_timeout", 10*time.Second, "crash after retrying etcd mutation for a specified duration")

	argEtcdServers  = flag.StringSlice("etcd_servers", []string{"http://127.0.0.1:4001"}, "List of etcd servers to watch (http://ip:port), comma separated")
	argEtcdCAFile   = flag.String("etcd_cafile", "", "SSL Certificate Authority file used to secure etcd communication")
	argEtcdCertFile = flag.String("etcd_certfile", "", "SSL certification file used to secure etcd communication")
	argEtcdKeyFile  = flag.String("etcd_keyfile", "", "SSL key file used to secure etcd communication")
	argEtcdQuorum   = flag.Bool("etcd_quorum_read", false, "If true, enable quorum read")
	argEtcdPrefix   = flag.String("etcd_prefix", "", "backend(etcd) path prefix")

	argKubecfgFile   = flag.String("kubecfg_file", "", "Location of kubecfg file for access to kubernetes master service; --kube_master_url overrides the URL part of this; if neither this nor --kube_master_url are provided, defaults to service account tokens")
	argKubeMasterURL = flag.String("kube_master_url", "", "URL to reach kubernetes master. Env variables in this flag will be expanded.")
	argKubeUserName  = flag.String("kube_username", "", "Basic authorizaiton username")
	argKubePassword  = flag.String("kube_password", "", "Basic authorization password")
	argInsecureCert  = flag.Bool("kube_insecure_cert", false, "Flag of user inscecure cert")
)

const (
	// pollInterval defines the amount of time between etcd connection attempts
	pollInterval = 5 * time.Second
	// pollTimeout defines the total amount of time to attempt connecting to etcd
	pollTimeout = 60 * time.Second
	// Resync period for the kube controller loop.
	resyncPeriod = 30 * time.Minute
	// A subdomain added to the user specified domain for all services.
	serviceSubdomain = "svc"
	// A subdomain added to the user specified dmoain for all pods.
	podSubdomain = "pod"
)

type etcdKeysAPI interface {
	Set(ctx context.Context, key, value string, options *etcd.SetOptions) (*etcd.Response, error)
	Get(ctx context.Context, key string, options *etcd.GetOptions) (*etcd.Response, error)
	Delete(ctx context.Context, key string, options *etcd.DeleteOptions) (*etcd.Response, error)
	Create(ctx context.Context, key, value string) (*etcd.Response, error)
	CreateInOrder(ctx context.Context, dir, value string, opts *etcd.CreateInOrderOptions) (*etcd.Response, error)
	Update(ctx context.Context, key, value string) (*etcd.Response, error)
	Watcher(key string, opts *etcd.WatcherOptions) *etcd.Watcher
}

type nameNamespace struct {
	name      string
	namespace string
}

type kube2sky struct {
	// Etcd client.
	etcdKeysAPI etcd.KeysAPI
	// DNS domain name.
	domain string
	// Etcd mutation timeout.
	etcdMutationTimeout time.Duration
	// A cache that contains all the endpoints in the system.
	endpointsStore kcache.Store
	// A cache that contains all the services in the system.
	servicesStore kcache.Store
	// openxxs
	// A cache that contains all the pods in the system.
	podsStore kcache.Store
	// A cache that contains all the nodes in the system.
	nodesStore kcache.Store
	// end openxxs
	// Lock for controlling access to headless services.
	mlock sync.Mutex
	// If true, perform quorum read.
	quorum bool
}

// Removes 'subdomain' from etcd.
func (ks *kube2sky) removeDNS(subdomain string) error {
	glog.V(2).Infof("Removing %s from DNS", subdomain)
	ctx := context.Background()
	opts := &etcd.GetOptions{
		Recursive: true,
		Quorum:    ks.quorum,
	}
	resp, err := ks.etcdKeysAPI.Get(ctx, skymsg.Path(subdomain), opts)
	if err != nil {
		return err
	}
	if emptyResponse(resp) {
		glog.V(2).Infof("Subdomain %q does not exist in etcd", subdomain)
		return nil
	}
	deleteOpts := &etcd.DeleteOptions{
		Recursive: true,
	}
	_, err = ks.etcdKeysAPI.Delete(ctx, skymsg.Path(subdomain), deleteOpts)
	return err
}

func (ks *kube2sky) writeSkyRecord(subdomain string, data string) error {
	// Set with no TTL, and hope that kubernetes events are accurate.
	ctx := context.Background()
	opts := &etcd.SetOptions{}
	_, err := ks.etcdKeysAPI.Set(ctx, skymsg.Path(subdomain), data, opts)
	return err
}

// Generates skydns records for a headless service.
func (ks *kube2sky) newHeadlessService(subdomain string, service *kapi.Service) error {
	// Create an A record for every pod in the service.
	// This record must be periodically updated.
	// Format is as follows:
	// For a service x, with pods a and b create DNS records,
	// a.x.ns.domain. and, b.x.ns.domain.
	ks.mlock.Lock()
	defer ks.mlock.Unlock()
	key, err := kcache.MetaNamespaceKeyFunc(service)
	if err != nil {
		return err
	}
	e, exists, err := ks.endpointsStore.GetByKey(key)
	if err != nil {
		return fmt.Errorf("failed to get endpoints object from endpoints store - %v", err)
	}
	if !exists {
		glog.V(1).Infof("Could not find endpoints for service %q in namespace %q. DNS records will be created once endpoints show up.", service.Name, service.Namespace)
		return nil
	}
	if e, ok := e.(*kapi.Endpoints); ok {
		return ks.generateRecordsForHeadlessService(subdomain, e, service)
	}
	return nil
}

func emptyResponse(resp *etcd.Response) bool {
	return resp == nil || resp.Node == nil || len(resp.Node.Value) == 0
}

func getSkyMsg(ip string, port int) *skymsg.Service {
	return &skymsg.Service{
		Host:     ip,
		Port:     port,
		Priority: 10,
		Weight:   10,
		Ttl:      30,
	}
}

func (ks *kube2sky) generateRecordsForHeadlessService(subdomain string, e *kapi.Endpoints, svc *kapi.Service) error {
	for idx := range e.Subsets {
		for subIdx := range e.Subsets[idx].Addresses {
			endpointIP := e.Subsets[idx].Addresses[subIdx].IP
			b, err := json.Marshal(getSkyMsg(endpointIP, 0))
			if err != nil {
				return err
			}
			recordValue := string(b)
			recordLabel := getHash(recordValue)
			if serializedPodHostnames := e.Annotations[endpoints.PodHostnamesAnnotation]; len(serializedPodHostnames) > 0 {
				podHostnames := map[string]endpoints.HostRecord{}
				err := json.Unmarshal([]byte(serializedPodHostnames), &podHostnames)
				if err != nil {
					return err
				}
				if hostRecord, exists := podHostnames[string(endpointIP)]; exists {
					if validation.IsDNS1123Label(hostRecord.HostName) {
						recordLabel = hostRecord.HostName
					}
				}
			}
			recordKey := buildDNSNameString(subdomain, recordLabel)

			glog.V(2).Infof("Setting DNS record: %v -> %q\n", recordKey, recordValue)
			if err := ks.writeSkyRecord(recordKey, recordValue); err != nil {
				return err
			}
			for portIdx := range e.Subsets[idx].Ports {
				endpointPort := &e.Subsets[idx].Ports[portIdx]
				portSegment := buildPortSegmentString(endpointPort.Name, endpointPort.Protocol)
				if portSegment != "" {
					err := ks.generateSRVRecord(subdomain, portSegment, recordLabel, recordKey, endpointPort.Port)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

func (ks *kube2sky) getServiceFromEndpoints(e *kapi.Endpoints) (*kapi.Service, error) {
	key, err := kcache.MetaNamespaceKeyFunc(e)
	if err != nil {
		return nil, err
	}
	obj, exists, err := ks.servicesStore.GetByKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to get service object from services store - %v", err)
	}
	if !exists {
		glog.V(1).Infof("could not find service for endpoint %q in namespace %q", e.Name, e.Namespace)
		return nil, nil
	}
	if svc, ok := obj.(*kapi.Service); ok {
		return svc, nil
	}
	return nil, fmt.Errorf("got a non service object in services store %v", obj)
}

func (ks *kube2sky) addDNSUsingEndpoints(subdomain string, e *kapi.Endpoints) error {
	ks.mlock.Lock()
	defer ks.mlock.Unlock()
	svc, err := ks.getServiceFromEndpoints(e)
	if err != nil {
		return err
	}
	if svc == nil || kapi.IsServiceIPSet(svc) {
		// No headless service found corresponding to endpoints object.
		return nil
	}
	// Remove existing DNS entry.
	if err := ks.removeDNS(subdomain); err != nil {
		return err
	}
	return ks.generateRecordsForHeadlessService(subdomain, e, svc)
}

func (ks *kube2sky) handleEndpointAdd(obj interface{}) {
	if e, ok := obj.(*kapi.Endpoints); ok {
		name := buildDNSNameString(ks.domain, serviceSubdomain, e.Namespace, e.Name)
		ks.mutateEtcdOrDie(func() error { return ks.addDNSUsingEndpoints(name, e) })
	}
}

func (ks *kube2sky) handlePodCreate(obj interface{}) {
	if e, ok := obj.(*kapi.Pod); ok {
		// If the pod ip is not yet available, do not attempt to create.
		if e.Status.PodIP != "" {
			// openxxs remove use IP as pod name
			// name := buildDNSNameString(ks.domain, podSubdomain, e.Namespace, santizeIP(e.Status.PodIP))
			// ks.mutateEtcdOrDie(func() error { return ks.generateRecordsForPod(name, e) })
			// openxxs: end
		}
		// openxxs: add pod hostname dns record when pod created
		if e.Name != "" {
			name2 := buildDNSNameString(ks.domain, e.Name)
			ks.mutateEtcdOrDie(func() error { return ks.generateRecordsForPod(name2, e) })
		}
		// openxxs: end
	}
}

func (ks *kube2sky) handlePodUpdate(old interface{}, new interface{}) {
	oldPod, okOld := old.(*kapi.Pod)
	newPod, okNew := new.(*kapi.Pod)

	// Validate that the objects are good
	if okOld && okNew {
		if oldPod.Status.PodIP != newPod.Status.PodIP {
			ks.handlePodDelete(oldPod)
			ks.handlePodCreate(newPod)
		}
	} else if okNew {
		ks.handlePodCreate(newPod)
	} else if okOld {
		ks.handlePodDelete(oldPod)
	}
}

func (ks *kube2sky) handlePodDelete(obj interface{}) {
	if e, ok := obj.(*kapi.Pod); ok {
		// openxxs remove use IP as pod name
		//if e.Status.PodIP != "" {
		// name := buildDNSNameString(ks.domain, podSubdomain, e.Namespace, santizeIP(e.Status.PodIP))
		// ks.mutateEtcdOrDie(func() error { return ks.removeDNS(name) })
		// openxxs: end
		if e.Name != "" {
			name := buildDNSNameString(ks.domain, e.Name)
			ks.mutateEtcdOrDie(func() error { return ks.removeDNS(name) })
		}
	}
}

// openxxs
func (ks *kube2sky) handleNodeCreate(obj interface{}) {
	if e, ok := obj.(*kapi.Node); ok {
		//internalIP := getNodeInternalIP(e)
		if e.Name != "" {
			name := buildDNSNameString(ks.domain, e.Name)
			// openxxs-test
			// glog.Infof("Node create DNS name: %v\n", name)
			// openxxs-test: end
			ks.mutateEtcdOrDie(func() error { return ks.generateRecordsForNode(name, e) })
		}
	}
}

func (ks *kube2sky) handleNodeDelete(obj interface{}) {
	if e, ok := obj.(*kapi.Node); ok {
		//internalIP := getNodeInternalIP(e)
		if e.Name != "" {
			name := buildDNSNameString(ks.domain, e.Name)
			// openxxs-test
			// glog.Infof("Node delete DNS name: %v\n", name)
			// openxxs-test: end
			ks.mutateEtcdOrDie(func() error { return ks.removeDNS(name) })
		}
	}
}

func (ks *kube2sky) handleNodeUpdate(old interface{}, new interface{}) {
	oldNode, okOld := old.(*kapi.Node)
	newNode, okNew := new.(*kapi.Node)
	if okOld && okNew {
		oldInternalIP := getNodeInternalIP(oldNode)
		newInternalIP := getNodeInternalIP(newNode)
		if oldInternalIP != newInternalIP {
			ks.handleNodeDelete(oldNode)
			ks.handleNodeCreate(newNode)
		}
	} else if okNew {
		// openxxs-test
		//glog.Infof("Node update okNew and !okOld\n")
		// openxxs-test: end
		ks.handleNodeCreate(newNode)
	} else if okOld {
		// openxxs-test
		//glog.Infof("Node update !okNew and okOld\n")
		// openxxs-test: end
		ks.handleNodeDelete(oldNode)
	}
}

func (ks *kube2sky) generateRecordsForNode(subdomain string, node *kapi.Node) error {
	b, err := json.Marshal(getSkyMsg(getNodeInternalIP(node), 0))
	if err != nil {
		return err
	}
	recordValue := string(b)
	recordLabel := getHash(recordValue)
	recordKey := buildDNSNameString(subdomain, recordLabel)
	glog.V(2).Infof("Setting DNS record: %v -> %q, with recordKey: %v\n", subdomain, recordValue, recordKey)
	// openxxs-test
	// glog.Infof("Setting DNS record: %v -> %q, with recordKey: %v\n", subdomain, recordValue, recordKey)
	// openxxs-test: end
	if err := ks.writeSkyRecord(recordKey, recordValue); err != nil {
		return err
	}
	return nil
}

// end openxxs

func (ks *kube2sky) generateRecordsForPod(subdomain string, service *kapi.Pod) error {
	b, err := json.Marshal(getSkyMsg(service.Status.PodIP, 0))
	if err != nil {
		return err
	}
	recordValue := string(b)
	recordLabel := getHash(recordValue)
	recordKey := buildDNSNameString(subdomain, recordLabel)

	glog.V(2).Infof("Setting DNS record: %v -> %q, with recordKey: %v\n", subdomain, recordValue, recordKey)
	if err := ks.writeSkyRecord(recordKey, recordValue); err != nil {
		return err
	}

	return nil
}

func (ks *kube2sky) generateRecordsForPortalService(subdomain string, service *kapi.Service) error {
	b, err := json.Marshal(getSkyMsg(service.Spec.ClusterIP, 0))
	if err != nil {
		return err
	}
	recordValue := string(b)
	recordLabel := getHash(recordValue)
	recordKey := buildDNSNameString(subdomain, recordLabel)

	glog.V(2).Infof("Setting DNS record: %v -> %q, with recordKey: %v\n", subdomain, recordValue, recordKey)
	if err := ks.writeSkyRecord(recordKey, recordValue); err != nil {
		return err
	}
	// Generate SRV Records
	for i := range service.Spec.Ports {
		port := &service.Spec.Ports[i]
		portSegment := buildPortSegmentString(port.Name, port.Protocol)
		if portSegment != "" {
			err = ks.generateSRVRecord(subdomain, portSegment, recordLabel, subdomain, port.Port)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func santizeIP(ip string) string {
	return strings.Replace(ip, ".", "-", -1)
}

func buildPortSegmentString(portName string, portProtocol kapi.Protocol) string {
	if portName == "" {
		// we don't create a random name
		return ""
	}

	if portProtocol == "" {
		glog.Errorf("Port Protocol not set. port segment string cannot be created.")
		return ""
	}

	return fmt.Sprintf("_%s._%s", portName, strings.ToLower(string(portProtocol)))
}

func (ks *kube2sky) generateSRVRecord(subdomain, portSegment, recordName, cName string, portNumber int) error {
	recordKey := buildDNSNameString(subdomain, portSegment, recordName)
	srv_rec, err := json.Marshal(getSkyMsg(cName, portNumber))
	if err != nil {
		return err
	}
	if err := ks.writeSkyRecord(recordKey, string(srv_rec)); err != nil {
		return err
	}
	return nil
}

func (ks *kube2sky) addDNS(subdomain string, service *kapi.Service) error {
	if len(service.Spec.Ports) == 0 {
		glog.Fatalf("Unexpected service with no ports: %v", service)
	}
	// if ClusterIP is not set, a DNS entry should not be created
	if !kapi.IsServiceIPSet(service) {
		return ks.newHeadlessService(subdomain, service)
	}
	return ks.generateRecordsForPortalService(subdomain, service)
}

// Implements retry logic for arbitrary mutator. Crashes after retrying for
// etcd_mutation_timeout.
func (ks *kube2sky) mutateEtcdOrDie(mutator func() error) {
	timeout := time.After(ks.etcdMutationTimeout)
	for {
		select {
		case <-timeout:
			glog.Fatalf("Failed to mutate etcd for %v using mutator: %v", ks.etcdMutationTimeout, mutator)
		default:
			if err := mutator(); err != nil {
				delay := 50 * time.Millisecond
				glog.V(1).Infof("Failed to mutate etcd using mutator: %v due to: %v. Will retry in: %v", mutator, err, delay)
				time.Sleep(delay)
			} else {
				return
			}
		}
	}
}

func buildDNSNameString(labels ...string) string {
	var res string
	for _, label := range labels {
		if res == "" {
			res = label
		} else {
			res = fmt.Sprintf("%s.%s", label, res)
		}
	}
	return res
}

// Returns a cache.ListWatch that gets all changes to services.
func createServiceLW(kubeClient *kclient.Client) *kcache.ListWatch {
	return kcache.NewListWatchFromClient(kubeClient, "services", kapi.NamespaceAll, kselector.Everything())
}

// Returns a cache.ListWatch that gets all changes to endpoints.
func createEndpointsLW(kubeClient *kclient.Client) *kcache.ListWatch {
	return kcache.NewListWatchFromClient(kubeClient, "endpoints", kapi.NamespaceAll, kselector.Everything())
}

// Returns a cache.ListWatch that gets all changes to pods.
func createEndpointsPodLW(kubeClient *kclient.Client) *kcache.ListWatch {
	return kcache.NewListWatchFromClient(kubeClient, "pods", kapi.NamespaceAll, kselector.Everything())
}

// openxxs: returns a cache.ListWatch that gets all changes to hostname of minions
func createNodesLW(kubeClient *kclient.Client) *kcache.ListWatch {
	return kcache.NewListWatchFromClient(kubeClient, "nodes", kapi.NamespaceAll, kselector.Everything())
}

// end openxxs

func (ks *kube2sky) newService(obj interface{}) {
	if s, ok := obj.(*kapi.Service); ok {
		name := buildDNSNameString(ks.domain, serviceSubdomain, s.Namespace, s.Name)
		ks.mutateEtcdOrDie(func() error { return ks.addDNS(name, s) })
	}
}

func (ks *kube2sky) removeService(obj interface{}) {
	if s, ok := obj.(*kapi.Service); ok {
		name := buildDNSNameString(ks.domain, serviceSubdomain, s.Namespace, s.Name)
		ks.mutateEtcdOrDie(func() error { return ks.removeDNS(name) })
	}
}

func (ks *kube2sky) updateService(oldObj, newObj interface{}) {
	// TODO: Avoid unwanted updates.
	ks.removeService(oldObj)
	ks.newService(newObj)
}

func newEtcdClient(config *etcdstorage.EtcdConfig) (client etcd.Client, err error) {
	ctx := context.Background()
	// loop until we have > 0 machines && machines[0] != ""
	poll, timeout := pollInterval, pollTimeout
	if err := wait.Poll(poll, timeout, func() (bool, error) {
		if client, err = config.NewEtcdClient(); err != nil {
			glog.V(2).Infof("cannot create etcd client: %v", err)
			return false, nil
		}
		if err = client.Sync(ctx); err != nil {
			glog.V(2).Infof("cannot sync cluster: %v", err)
			return false, nil
		}
		machines := client.Endpoints()
		if len(machines) == 0 || len(machines[0]) == 0 {
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("Timed out after %s waiting for at least 1 synchronized etcd server in the cluster. Error: %v", timeout, err)
	}
	return client, nil
}

func expandKubeMasterURL() (string, error) {

	parsedURL, err := url.Parse(os.ExpandEnv(*argKubeMasterURL))
	if err != nil {
		return "", fmt.Errorf("failed to parse --kube_master_url %s - %v", *argKubeMasterURL, err)
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" || parsedURL.Host == ":" {
		return "", fmt.Errorf("invalid --kube_master_url specified %s", *argKubeMasterURL)
	}
	return parsedURL.String(), nil
}

func expandAuthInfo() (string, string) {
	username := os.ExpandEnv(*argKubeUserName)
	password := os.ExpandEnv(*argKubePassword)
	return username, password

}

// TODO: evaluate using pkg/client/clientcmd
func newKubeClient() (*kclient.Client, error) {
	var (
		config    *restclient.Config
		err       error
		masterURL string
		username  string
		password  string
		insecure  bool
	)
	// If the user specified --kube_master_url, expand env vars and verify it.
	if *argKubeMasterURL != "" {
		masterURL, err = expandKubeMasterURL()
		if err != nil {
			return nil, err
		}
	}

	if *argKubeUserName != "" && *argKubePassword != "" {
		username, password = expandAuthInfo()
	}
	insecure = *argInsecureCert

	if masterURL != "" && *argKubecfgFile == "" {
		// Only --kube_master_url was provided.
		if username != "" && password != "" {
			config = &restclient.Config{
				Host:          masterURL,
				Username:      username,
				Password:      password,
				Insecure:      insecure,
				ContentConfig: restclient.ContentConfig{GroupVersion: &unversioned.GroupVersion{Version: "v1"}},
			}
		} else {
			config = &restclient.Config{
				Host:          masterURL,
				ContentConfig: restclient.ContentConfig{GroupVersion: &unversioned.GroupVersion{Version: "v1"}},
			}
		}

	} else {
		// We either have:
		//  1) --kube_master_url and --kubecfg_file
		//  2) just --kubecfg_file
		//  3) neither flag
		// In any case, the logic is the same.  If (3), this will automatically
		// fall back on the service account token.
		overrides := &kclientcmd.ConfigOverrides{}
		overrides.ClusterInfo.Server = masterURL // might be "", but that is OK
		overrides.ClusterInfo.InsecureSkipTLSVerify = insecure
		overrides.AuthInfo.Username = username
		overrides.AuthInfo.Password = password
		rules := &kclientcmd.ClientConfigLoadingRules{ExplicitPath: *argKubecfgFile} // might be "", but that is OK
		if config, err = kclientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig(); err != nil {
			return nil, err
		}
	}

	glog.Infof("Using %s for kubernetes master", config.Host)
	glog.Infof("Using kubernetes API %v", config.GroupVersion)
	return kclient.New(config)
}

func watchForServices(kubeClient *kclient.Client, ks *kube2sky) kcache.Store {
	serviceStore, serviceController := kframework.NewInformer(
		createServiceLW(kubeClient),
		&kapi.Service{},
		resyncPeriod,
		kframework.ResourceEventHandlerFuncs{
			AddFunc:    ks.newService,
			DeleteFunc: ks.removeService,
			UpdateFunc: ks.updateService,
		},
	)
	go serviceController.Run(wait.NeverStop)
	return serviceStore
}

func watchEndpoints(kubeClient *kclient.Client, ks *kube2sky) kcache.Store {
	eStore, eController := kframework.NewInformer(
		createEndpointsLW(kubeClient),
		&kapi.Endpoints{},
		resyncPeriod,
		kframework.ResourceEventHandlerFuncs{
			AddFunc: ks.handleEndpointAdd,
			UpdateFunc: func(oldObj, newObj interface{}) {
				// TODO: Avoid unwanted updates.
				ks.handleEndpointAdd(newObj)
			},
		},
	)

	go eController.Run(wait.NeverStop)
	return eStore
}

func watchPods(kubeClient *kclient.Client, ks *kube2sky) kcache.Store {
	eStore, eController := kframework.NewInformer(
		createEndpointsPodLW(kubeClient),
		&kapi.Pod{},
		resyncPeriod,
		kframework.ResourceEventHandlerFuncs{
			AddFunc: ks.handlePodCreate,
			UpdateFunc: func(oldObj, newObj interface{}) {
				ks.handlePodUpdate(oldObj, newObj)
			},
			DeleteFunc: ks.handlePodDelete,
		},
	)

	go eController.Run(wait.NeverStop)
	return eStore
}

// openxxs
func watchNodes(kubeClient *kclient.Client, ks *kube2sky) kcache.Store {
	eStore, eController := kframework.NewInformer(
		createNodesLW(kubeClient),
		&kapi.Node{},
		resyncPeriod,
		kframework.ResourceEventHandlerFuncs{
			AddFunc: ks.handleNodeCreate,
			UpdateFunc: func(oldObj, newObj interface{}) {
				ks.handleNodeUpdate(oldObj, newObj)
			},
			DeleteFunc: ks.handleNodeDelete,
		},
	)

	go eController.Run(wait.NeverStop)
	return eStore
}

// end openxxs

func getHash(text string) string {
	h := fnv.New32a()
	h.Write([]byte(text))
	return fmt.Sprintf("%x", h.Sum32())
}

// openxxs
func getNodeInternalIP(node *kapi.Node) string {
	for _, address := range node.Status.Addresses {
		if address.Type == "InternalIP" {
			return address.Address
		}
	}
	return ""
}

// openxxs: end

// setupSignalHandlers runs a goroutine that waits on SIGINT or SIGTERM and logs it
// before exiting.
func setupSignalHandlers() {
	sigChan := make(chan os.Signal)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	// This program should always exit gracefully logging that it received
	// either a SIGINT or SIGTERM. Since kube2sky is run in a container
	// without a liveness probe as part of the kube-dns pod, it shouldn't
	// restart unless the pod is deleted. If it restarts without logging
	// anything it means something is seriously wrong.
	// TODO: Remove once #22290 is fixed.
	go func() {
		glog.Fatalf("Received signal %s", <-sigChan)
	}()
}

func main() {
	flag.CommandLine.AddGoFlagSet(goflag.CommandLine)
	flag.Parse()
	goflag.CommandLine.Parse([]string{})
	var err error
	setupSignalHandlers()
	domain := *argDomain
	if !strings.HasSuffix(domain, ".") {
		domain = fmt.Sprintf("%s.", domain)
	}
	if *argEtcdPrefix != "" {
		skymsg.PathPrefix = strings.Trim(*argEtcdPrefix, "/") + "/" + skymsg.PathPrefix
	}
	etcdConfig := &etcdstorage.EtcdConfig{
		ServerList: *argEtcdServers,
		Quorum:     *argEtcdQuorum,
		CAFile:     *argEtcdCAFile,
		CertFile:   *argEtcdCertFile,
		KeyFile:    *argEtcdKeyFile,
	}
	ks := kube2sky{
		domain:              domain,
		etcdMutationTimeout: *argEtcdMutationTimeout,
		quorum:              etcdConfig.Quorum,
	}
	var etcdClient etcd.Client
	if etcdClient, err = newEtcdClient(etcdConfig); err != nil {
		glog.Fatalf("Failed to create etcd client: %v", err)
	}
	ks.etcdKeysAPI = etcd.NewKeysAPI(etcdClient)

	kubeClient, err := newKubeClient()
	if err != nil {
		glog.Fatalf("Failed to create a kubernetes client: %v", err)
	}

	ks.endpointsStore = watchEndpoints(kubeClient, &ks)
	ks.servicesStore = watchForServices(kubeClient, &ks)
	// openxxs
	ks.podsStore = watchPods(kubeClient, &ks)
	ks.nodesStore = watchNodes(kubeClient, &ks)
	// end openxxs

	select {}
}
