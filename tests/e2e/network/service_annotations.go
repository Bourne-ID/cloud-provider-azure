/*
Copyright 2018 The Kubernetes Authors.

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

package network

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-07-01/network"
	"github.com/Azure/go-autorest/autorest/to"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/legacy-cloud-providers/azure"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var (
	scalesetRE                 = regexp.MustCompile(`.*/subscriptions/(?:.*)/resourceGroups/(.+)/providers/Microsoft.Compute/virtualMachineScaleSets/(.+)/virtualMachines(?:.*)`)
	lbNameRE                   = regexp.MustCompile(`^/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Network/loadBalancers/(.+)/frontendIPConfigurations(?:.*)`)
	backendIPConfigurationRE   = regexp.MustCompile(`^/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Compute/virtualMachineScaleSets/(.+)/virtualMachines(?:.*)`)
	backendASIPConfigurationRE = regexp.MustCompile(`^/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Compute/availabilitySets/(.+)/virtualMachines(?:.*)`)
)

const (
	nginxPort       = 80
	nginxStatusCode = 200
	pullInterval    = 20 * time.Second
	pullTimeout     = 10 * time.Minute
)

var _ = FDescribe("Service with annotation", func() {
	basename := "service"
	serviceName := "annotation-test"

	var cs clientset.Interface
	var ns *v1.Namespace

	labels := map[string]string{
		"app": serviceName,
	}
	ports := []v1.ServicePort{{
		Port:       nginxPort,
		TargetPort: intstr.FromInt(nginxPort),
	}}

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating deployment " + serviceName)
		deployment := createNginxDeploymentManifest(serviceName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(deployment)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		err := cs.AppsV1().Deployments(ns.Name).Delete(serviceName, nil)
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
	})

	It("can be accessed by domain name", func() {
		By("Create service")
		serviceDomainNamePrefix := serviceName + string(uuid.NewUUID())

		annotation := map[string]string{
			azure.ServiceAnnotationDNSLabelName: serviceDomainNamePrefix,
		}

		// create service with given annotation and wait it to expose
		_ = createServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)

		By("Validating External domain name")
		var code int
		serviceDomainName := utils.GetServiceDomainName(serviceDomainNamePrefix)
		url := fmt.Sprintf("http://%s:%v", serviceDomainName, ports[0].Port)
		for i := 1; i <= 30; i++ {
			resp, err := http.Get(url)
			if err == nil {
				defer func() {
					if resp != nil {
						resp.Body.Close()
					}
				}()
				code = resp.StatusCode
				if resp.StatusCode == nginxStatusCode {
					break
				}
			}
			time.Sleep(20 * time.Second)
		}
		Expect(code).To(Equal(nginxStatusCode), "Fail to get response from the domain name")
	})

	It("can be bound to an internal load balancer", func() {
		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerInternal: "true",
		}

		// create service with given annotation and wait it to expose
		ip := createServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)

		By("Validating whether the load balancer is internal")
		url := fmt.Sprintf("%s:%v", ip, ports[0].Port)
		err := validateInternalLoadBalancer(cs, ns.Name, url)
		Expect(err).NotTo(HaveOccurred())
	})

	It("can specify which subnet the internal load balancer should be bound to", func() {
		By("creating environment")
		subnetName := "lb-subnet"

		azureTestClient, err := utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())

		vNet, err := azureTestClient.GetClusterVirtualNetwork()
		Expect(err).NotTo(HaveOccurred())

		var newSubnetCIDR string
		for _, existingSubnet := range *vNet.Subnets {
			if *existingSubnet.Name == subnetName {
				By("Test subnet have existed, skip creating")
				newSubnetCIDR = *existingSubnet.AddressPrefix
				break
			}
		}

		if newSubnetCIDR == "" {
			By("Test subnet doesn't exist. Creating a new one...")
			newSubnetCIDR, err = utils.GetNextSubnetCIDR(vNet)
			Expect(err).NotTo(HaveOccurred())
			azureTestClient.CreateSubnet(vNet, &subnetName, &newSubnetCIDR)
		}

		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerInternal:       "true",
			azure.ServiceAnnotationLoadBalancerInternalSubnet: subnetName,
		}

		// create service with given annotation and wait it to expose
		ip := createServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		utils.Logf("Get External IP: %s", ip)

		By("Validating external ip in target subnet")
		ret, err := utils.ValidateIPInCIDR(ip, newSubnetCIDR)
		Expect(err).NotTo(HaveOccurred())
		Expect(ret).To(Equal(true), "external ip %s is not in the target subnet %s", ip, newSubnetCIDR)
	})

	It("should support load balancer annotation 'ServiceAnnotationLoadBalancerIdleTimeout'", func() {
		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerIdleTimeout: "5",
		}

		// create service with given annotation and wait it to expose
		publicIP := createServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)

		// get lb from azure client
		lb := getAzureLoadBalancer(publicIP)

		var idleTimeout *int32
		for _, rule := range *lb.LoadBalancingRules {
			if rule.IdleTimeoutInMinutes != nil {
				idleTimeout = rule.IdleTimeoutInMinutes
			}
		}
		Expect(*idleTimeout).To(Equal(int32(5)))
	})

	It("should support load balancer annotation 'ServiceAnnotationLoadBalancerMixedProtocols'", func() {
		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerMixedProtocols: "true",
		}

		// create service with given annotation and wait it to expose
		publicIP := createServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)

		// get lb from azure client
		lb := getAzureLoadBalancer(publicIP)

		existingProtocols := make(map[network.TransportProtocol]int)
		for _, rule := range *lb.LoadBalancingRules {
			if _, ok := existingProtocols[rule.Protocol]; !ok {
				existingProtocols[rule.Protocol]++
			}
		}
		Expect(len(existingProtocols)).To(Equal(2))
	})

	It("should support load balancer annotation 'ServiceAnnotationLoadBalancerResourceGroup'", func() {
		By("Creating Azure clients")
		tc, err := utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())

		By("creating a test resource group")
		rg, cleanup := utils.CreateTestResourceGroup(tc)
		defer cleanup(to.String(rg.Name))

		By("creating test PIP in the test resource group")
		testPIPName := "testPIP-" + string(uuid.NewUUID())[0:4]
		pip, err := utils.WaitCreateNewPIP(tc, testPIPName, *rg.Name, defaultPublicIPAddress(testPIPName))
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			By("Cleaning up")
			err = cs.CoreV1().Services(ns.Name).Delete(serviceName, nil)
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeletePIPWithRetry(tc, testPIPName, *rg.Name)
			Expect(err).NotTo(HaveOccurred())
		}()

		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerResourceGroup: to.String(rg.Name),
		}
		By("Creating service " + serviceName + " in namespace " + ns.Name)
		service := utils.CreateLoadBalancerServiceManifest(cs, serviceName, annotation, labels, ns.Name, ports)
		service.Spec.LoadBalancerIP = *pip.IPAddress
		_, err = cs.CoreV1().Services(ns.Name).Create(service)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns.Name)

		//wait and get service's public IP Address
		By("Waiting service to expose...")
		_, err = utils.WaitServiceExposure(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())

		lb := getAzureLoadBalancerFromPIP(*pip.IPAddress, *rg.Name, "")
		Expect(lb).NotTo(BeNil())
	})
})

var _ = Describe("[MultipleAgentPools][VMSS]", func() {
	basename := "service"
	serviceName := "annotation-test"

	var cs clientset.Interface
	var ns *v1.Namespace

	labels := map[string]string{
		"app": serviceName,
	}
	ports := []v1.ServicePort{{
		Port:       nginxPort,
		TargetPort: intstr.FromInt(nginxPort),
	}}

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating deployment " + serviceName)
		deployment := createNginxDeploymentManifest(serviceName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(deployment)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		err := cs.AppsV1().Deployments(ns.Name).Delete(serviceName, nil)
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
	})

	It("should bound to the specified load balancer from azure-load-balancer-mode", func() {
		//get nodelist and providerID specific to an agentnodes
		By("Getting agent nodes list")
		nodes, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())

		// Get all the vmss names from all node's providerIDs
		By("Get vmss names from node providerIDs")
		vmssNames := sets.NewString()
		var resourceGroupName string
		for _, node := range nodes {
			if utils.IsMasterNode(&node) {
				continue
			}
			providerID := node.Spec.ProviderID
			matches := scalesetRE.FindStringSubmatch(providerID)
			if len(matches) != 3 {
				Skip("azure-load-balancer-mode tests only works for vmss cluster")
			}
			resourceGroupName = matches[1]
			vmssNames.Insert(matches[2])
		}
		Expect(resourceGroupName).NotTo(Equal(""))
		utils.Logf("Got vmss names %v", vmssNames.List())

		//Skip if there're less than two vmss
		if len(vmssNames) < 2 {
			Skip("azure-load-balancer-mode tests only works for cluster with multiple vmss agent pools")
		}

		vmssList := vmssNames.List()[:2]
		for _, vmss := range vmssList {
			validateLoadBalancerBackendPools(vmss, cs, serviceName, labels, ns.Name, ports, resourceGroupName)
		}
	})
})

func getAzureLoadBalancer(pip string) *network.LoadBalancer {
	By("Creating Azure clients")
	azureTestClient, err := utils.CreateAzureTestClient()
	Expect(err).NotTo(HaveOccurred())

	resourceGroupName := azureTestClient.GetResourceGroup()
	By("Getting public IPs in the resourceGroup " + resourceGroupName)
	pipList, err := azureTestClient.ListPublicIPs(resourceGroupName)
	Expect(err).NotTo(HaveOccurred())

	By("Getting public IP frontend configuration ID")
	var pipFrontendConfigurationID string
	for _, ip := range pipList {
		if ip.PublicIPAddressPropertiesFormat != nil &&
			ip.PublicIPAddressPropertiesFormat.IPAddress != nil &&
			ip.PublicIPAddressPropertiesFormat.IPConfiguration != nil &&
			ip.PublicIPAddressPropertiesFormat.IPConfiguration.ID != nil &&
			*ip.PublicIPAddressPropertiesFormat.IPAddress == pip {
			pipFrontendConfigurationID = *ip.PublicIPAddressPropertiesFormat.IPConfiguration.ID
			break
		}
	}
	Expect(pipFrontendConfigurationID).NotTo(Equal(""))
	By(fmt.Sprintf("Successfully obtained PIP front config id: %v", pipFrontendConfigurationID))

	By("Getting loadBalancer name from pipFrontendConfigurationID")
	match := lbNameRE.FindStringSubmatch(pipFrontendConfigurationID)
	Expect(len(match)).To(Equal(2))
	loadBalancerName := match[1]
	Expect(loadBalancerName).NotTo(Equal(""))
	utils.Logf("Got loadBalancerName %q", loadBalancerName)

	By("Getting loadBalancer")
	lb, err := azureTestClient.GetLoadBalancer(resourceGroupName, loadBalancerName)
	Expect(err).NotTo(HaveOccurred())
	Expect(lb.LoadBalancingRules).NotTo(BeNil())

	return &lb
}

func getAzureLoadBalancerFromPIP(pip, pipResourceGroup, lbResourceGroup string) *network.LoadBalancer {
	By("Creating Azure clients")
	azureTestClient, err := utils.CreateAzureTestClient()
	Expect(err).NotTo(HaveOccurred())

	By("Getting public IPs in the resourceGroup " + pipResourceGroup)
	pipList, err := azureTestClient.ListPublicIPs(pipResourceGroup)
	Expect(err).NotTo(HaveOccurred())

	By("Getting public IP frontend configuration ID")
	var pipFrontendConfigurationID string
	for _, ip := range pipList {
		if ip.PublicIPAddressPropertiesFormat != nil &&
			ip.PublicIPAddressPropertiesFormat.IPAddress != nil &&
			ip.PublicIPAddressPropertiesFormat.IPConfiguration != nil &&
			ip.PublicIPAddressPropertiesFormat.IPConfiguration.ID != nil &&
			*ip.PublicIPAddressPropertiesFormat.IPAddress == pip {
			pipFrontendConfigurationID = *ip.PublicIPAddressPropertiesFormat.IPConfiguration.ID
			break
		}
	}
	Expect(pipFrontendConfigurationID).NotTo(Equal(""))
	By(fmt.Sprintf("Successfully obtained PIP front config id: %v", pipFrontendConfigurationID))

	By("Getting loadBalancer name from pipFrontendConfigurationID")
	match := lbNameRE.FindStringSubmatch(pipFrontendConfigurationID)
	Expect(len(match)).To(Equal(2))
	loadBalancerName := match[1]
	Expect(loadBalancerName).NotTo(Equal(""))
	utils.Logf("Got loadBalancerName %q", loadBalancerName)

	By("Getting loadBalancer")
	if lbResourceGroup == "" {
		lbResourceGroup = azureTestClient.GetResourceGroup()
	}
	lb, err := azureTestClient.GetLoadBalancer(lbResourceGroup, loadBalancerName)
	Expect(err).NotTo(HaveOccurred())
	Expect(lb.LoadBalancingRules).NotTo(BeNil())

	return &lb
}

func createServiceWithAnnotation(cs clientset.Interface, serviceName, nsName string, labels, annotation map[string]string, ports []v1.ServicePort) string {
	By("Creating service " + serviceName + " in namespace " + nsName)
	service := utils.CreateLoadBalancerServiceManifest(cs, serviceName, annotation, labels, nsName, ports)
	_, err := cs.CoreV1().Services(nsName).Create(service)
	Expect(err).NotTo(HaveOccurred())
	utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + nsName)

	//wait and get service's public IP Address
	By("Waiting service to expose...")
	publicIP, err := utils.WaitServiceExposure(cs, nsName, serviceName)
	Expect(err).NotTo(HaveOccurred())

	return publicIP
}

// defaultDeployment returns a default deployment
// running nginx image which exposes port 80
func createNginxDeploymentManifest(name string, labels map[string]string) (result *appsv1.Deployment) {
	var replicas int32
	replicas = 5
	result = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: v1.PodSpec{
					Hostname: name,
					Containers: []v1.Container{
						{
							Name:            "test-app",
							Image:           "nginx:1.15",
							ImagePullPolicy: "Always",
							Ports: []v1.ContainerPort{
								{
									ContainerPort: nginxPort,
								},
							},
						},
					},
				},
			},
		},
	}
	return
}

// validate internal source can access to ILB
func validateInternalLoadBalancer(c clientset.Interface, ns string, url string) error {
	// create a pod to access to the service
	utils.Logf("Validating external IP not be public and internal accessible")
	utils.Logf("Create a front pod to connect to service")
	podName := "front-pod"
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Hostname: podName,
			Containers: []v1.Container{
				{
					Name:            "test-app",
					Image:           "appropriate/curl",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command: []string{
						"/bin/sh",
						"-c",
						"code=0; while [ $code != 200 ]; do code=$(curl -s -o /dev/null -w \"%{http_code}\" " + url + "); sleep 1; done; echo $code",
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	_, err := c.CoreV1().Pods(ns).Create(pod)
	if err != nil {
		return err
	}
	defer func() {
		utils.Logf("Deleting front pod")
		err = utils.DeletePod(c, ns, podName)
	}()

	// publicFlag shows whether public accessible test ends
	// internalFlag shows whether internal accessible test ends
	utils.Logf("Call from the created pod")
	err = wait.PollImmediate(pullInterval, pullTimeout, func() (bool, error) {
		pod, err := c.CoreV1().Pods(ns).Get(podName, metav1.GetOptions{})
		if err != nil {
			if utils.IsRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		if pod.Status.Phase != v1.PodSucceeded {
			utils.Logf("waiting for the pod succeeded")
			return false, nil
		}
		if pod.Status.ContainerStatuses[0].State.Terminated == nil || pod.Status.ContainerStatuses[0].State.Terminated.Reason != "Completed" {
			utils.Logf("waiting for the container completed")
			return false, nil
		}
		utils.Logf("Still testing internal access from front pod to internal service")
		log, err := c.CoreV1().Pods(ns).GetLogs(pod.Name, &v1.PodLogOptions{}).Do().Raw()
		if err != nil {
			return false, nil
		}
		return strings.Contains(fmt.Sprintf("%s", log), "200"), nil
	})
	utils.Logf("validation finished")
	return err
}

func validateLoadBalancerBackendPools(vmssName string, cs clientset.Interface, serviceName string, labels map[string]string, ns string, ports []v1.ServicePort, resourceGroupName string) {
	serviceName = fmt.Sprintf("%s-%s", serviceName, vmssName)

	// create annotation for LoadBalancer service
	By("Creating service " + serviceName + " in namespace " + ns)
	annotation := map[string]string{
		azure.ServiceAnnotationLoadBalancerMode: vmssName,
	}
	service := utils.CreateLoadBalancerServiceManifest(cs, serviceName, annotation, labels, ns, ports)
	_, err := cs.CoreV1().Services(ns).Create(service)
	Expect(err).NotTo(HaveOccurred())
	utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns)

	//wait and get service's public IP Address
	By("Waiting for service exposure")
	publicIP, err := utils.WaitServiceExposure(cs, ns, serviceName)
	Expect(err).NotTo(HaveOccurred())

	By("Creating Azure clients")
	azureTestClient, err := utils.CreateAzureTestClient()
	Expect(err).NotTo(HaveOccurred())

	// Invoking azure network client to get list of public IP Addresses
	By("Getting public IPs in the resourceGroup " + resourceGroupName)
	pipList, err := azureTestClient.ListPublicIPs(resourceGroupName)
	Expect(err).NotTo(HaveOccurred())

	By("Getting public IP frontend configuration ID")
	var pipFrontendConfigurationID string
	for _, ip := range pipList {
		if ip.PublicIPAddressPropertiesFormat != nil &&
			ip.PublicIPAddressPropertiesFormat.IPAddress != nil &&
			ip.PublicIPAddressPropertiesFormat.IPConfiguration != nil &&
			ip.PublicIPAddressPropertiesFormat.IPConfiguration.ID != nil &&
			*ip.PublicIPAddressPropertiesFormat.IPAddress == publicIP {
			pipFrontendConfigurationID = *ip.PublicIPAddressPropertiesFormat.IPConfiguration.ID
			break
		}
	}
	Expect(pipFrontendConfigurationID).NotTo(Equal(""))

	//Get Azure loadBalancer Name
	By("Getting loadBalancer name from pipFrontendConfigurationID")
	match := lbNameRE.FindStringSubmatch(pipFrontendConfigurationID)
	Expect(len(match)).To(Equal(2))
	loadBalancerName := match[1]
	Expect(loadBalancerName).NotTo(Equal(""))
	utils.Logf("Got loadBalancerName %q", loadBalancerName)

	//Get backendpools list
	By("Getting loadBalancer")
	lb, err := azureTestClient.GetLoadBalancer(resourceGroupName, loadBalancerName)
	Expect(err).NotTo(HaveOccurred())
	Expect(lb.BackendAddressPools).NotTo(BeNil())
	Expect(lb.LoadBalancingRules).NotTo(BeNil())

	if lb.Sku != nil && lb.Sku.Name == network.LoadBalancerSkuNameStandard {
		Skip("azure-load-balancer-mode is not working for standard load balancer")
	}

	By("Getting loadBalancer backendPoolID")
	backendPoolID := ""
	for _, rule := range *lb.LoadBalancingRules {
		if rule.FrontendIPConfiguration != nil &&
			rule.FrontendIPConfiguration.ID != nil &&
			strings.EqualFold(*rule.FrontendIPConfiguration.ID, pipFrontendConfigurationID) {
			Expect(rule.BackendAddressPool).NotTo(BeNil())
			Expect(rule.BackendAddressPool.ID).NotTo(BeNil())
			backendPoolID = *rule.BackendAddressPool.ID
		}
	}
	Expect(backendPoolID).NotTo(Equal(""))

	By("Validating loadBalancer backendPool")
	for _, pool := range *lb.BackendAddressPools {
		if pool.ID == nil || pool.BackendIPConfigurations == nil || !strings.EqualFold(*pool.ID, backendPoolID) {
			continue
		}

		for _, ipConfig := range *pool.BackendIPConfigurations {
			if ipConfig.ID == nil {
				continue
			}

			matches := backendIPConfigurationRE.FindStringSubmatch(*ipConfig.ID)
			Expect(len(matches)).To(Equal(2))
			Expect(matches[1]).To(Equal(vmssName))
		}
	}
}
