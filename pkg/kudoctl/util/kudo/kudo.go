package kudo

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kudobuilder/kudo/pkg/apis/kudo/v1alpha1"
	"github.com/kudobuilder/kudo/pkg/client/clientset/versioned"
	"github.com/kudobuilder/kudo/pkg/kudoctl/clog"
	"github.com/kudobuilder/kudo/pkg/util/kudo"
	"github.com/kudobuilder/kudo/pkg/version"

	"github.com/pkg/errors"
	v1core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"

	// Import Kubernetes authentication providers to support GKE, etc.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"
)

// Client is a KUDO Client providing access to a clientset
type Client struct {
	clientset versioned.Interface
}

// NewClient creates new KUDO Client
func NewClient(namespace, kubeConfigPath string) (*Client, error) {

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath)
	if err != nil {
		return nil, err
	}

	// set default configs
	config.Timeout = time.Second * 3

	// create the clientset
	kudoClientset, err := versioned.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	_, err = kudoClientset.KudoV1alpha1().Operators(namespace).List(v1.ListOptions{})
	if err != nil {
		return nil, errors.WithMessage(err, "operators")
	}
	_, err = kudoClientset.KudoV1alpha1().OperatorVersions(namespace).List(v1.ListOptions{})
	if err != nil {
		return nil, errors.WithMessage(err, "operatorversions")
	}
	_, err = kudoClientset.KudoV1alpha1().Instances(namespace).List(v1.ListOptions{})
	if err != nil {
		return nil, errors.WithMessage(err, "instances")
	}

	return &Client{
		clientset: kudoClientset,
	}, nil
}

// NewClientFromK8s creates KUDO client from kubernetes client interface
func NewClientFromK8s(client versioned.Interface) *Client {
	result := Client{}
	result.clientset = client
	return &result
}

// OperatorExistsInCluster checks if a given Operator object is installed on the current k8s cluster
func (c *Client) OperatorExistsInCluster(name, namespace string) bool {
	operator, err := c.clientset.KudoV1alpha1().Operators(namespace).Get(name, v1.GetOptions{})
	if err != nil {
		clog.V(2).Printf("operator.kudo.dev/%s does not exist\n", name)
		return false
	}
	clog.V(2).Printf("operator.kudo.dev/%s unchanged", operator.Name)
	return true
}

// InstanceExistsInCluster checks if any OperatorVersion object matches to the given Operator name
// in the cluster.
// An Instance has two identifiers:
// 		1) Spec.OperatorVersion.Name
// 		spec:
//    		operatorVersion:
//      		name: kafka-2.11-2.4.0
// 		2) LabelSelector
// 		metadata:
//    		creationTimestamp: "2019-02-28T14:39:20Z"
//    		generation: 1
//    		labels:
//      		controller-tools.k8s.io: "1.0"
//      		kudo.dev/operator: kafka
// This function also just returns true if the Instance matches a specific OperatorVersion of an Operator
func (c *Client) InstanceExistsInCluster(operatorName, namespace, version, instanceName string) (bool, error) {
	instances, err := c.clientset.KudoV1alpha1().Instances(namespace).List(v1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", kudo.OperatorLabel, operatorName)})
	if err != nil {
		return false, err
	}
	if len(instances.Items) < 1 {
		return false, nil
	}

	// TODO: check function that actual checks for the OperatorVersion named e.g. "test-1.0" to exist
	var i int
	for _, v := range instances.Items {
		if v.Spec.OperatorVersion.Name == operatorName+"-"+version && v.ObjectMeta.Name == instanceName {
			i++
		}
	}

	// No instance exist with this operatorName and OV exists
	if i == 0 {
		return false, nil
	}
	return true, nil
}

// GetInstance queries kubernetes api for instance of given name in given namespace
// returns error for error conditions. Instance not found is not considered an error and will result in 'nil, nil'
func (c *Client) GetInstance(name, namespace string) (*v1alpha1.Instance, error) {
	instance, err := c.clientset.KudoV1alpha1().Instances(namespace).Get(name, v1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return instance, err
}

// GetOperatorVersion queries kubernetes api for operatorversion of given name in given namespace
// returns error for all other errors that not found, not found is treated as result being 'nil, nil'
func (c *Client) GetOperatorVersion(name, namespace string) (*v1alpha1.OperatorVersion, error) {
	ov, err := c.clientset.KudoV1alpha1().OperatorVersions(namespace).Get(name, v1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return ov, err
}

// UpdateInstance updates operatorversion on instance
func (c *Client) UpdateInstance(instanceName, namespace string, operatorVersionName *string, parameters map[string]string) error {
	instanceSpec := v1alpha1.InstanceSpec{}
	if operatorVersionName != nil {
		instanceSpec.OperatorVersion = v1core.ObjectReference{
			Name: kudo.StringValue(operatorVersionName),
		}
	}
	if parameters != nil {
		instanceSpec.Parameters = parameters
	}
	serializedPatch, err := json.Marshal(struct {
		Spec *v1alpha1.InstanceSpec `json:"spec"`
	}{
		&instanceSpec,
	})
	if err != nil {
		return err
	}
	_, err = c.clientset.KudoV1alpha1().Instances(namespace).Patch(instanceName, types.MergePatchType, serializedPatch)
	return err
}

// ListInstances lists all instances of given operator installed in the cluster in a given ns
func (c *Client) ListInstances(namespace string) ([]string, error) {
	instances, err := c.clientset.KudoV1alpha1().Instances(namespace).List(v1.ListOptions{})
	if err != nil {
		return nil, err
	}
	existingInstances := []string{}

	for _, v := range instances.Items {
		existingInstances = append(existingInstances, v.Name)
	}
	return existingInstances, nil
}

// OperatorVersionsInstalled lists all the versions of given operator installed in the cluster in given ns
func (c *Client) OperatorVersionsInstalled(operatorName, namespace string) ([]string, error) {
	ov, err := c.clientset.KudoV1alpha1().OperatorVersions(namespace).List(v1.ListOptions{})
	if err != nil {
		return nil, err
	}
	existingVersions := []string{}

	for _, v := range ov.Items {
		if strings.HasPrefix(v.Name, operatorName) {
			existingVersions = append(existingVersions, v.Spec.Version)
		}
	}
	return existingVersions, nil
}

// InstallOperatorObjToCluster expects a valid Operator obj to install
func (c *Client) InstallOperatorObjToCluster(obj *v1alpha1.Operator, namespace string) (*v1alpha1.Operator, error) {
	createdObj, err := c.clientset.KudoV1alpha1().Operators(namespace).Create(obj)
	if err != nil {
		return nil, errors.WithMessage(err, "installing Operator")
	}
	return createdObj, nil
}

// InstallOperatorVersionObjToCluster expects a valid Operator obj to install
func (c *Client) InstallOperatorVersionObjToCluster(obj *v1alpha1.OperatorVersion, namespace string) (*v1alpha1.OperatorVersion, error) {
	createdObj, err := c.clientset.KudoV1alpha1().OperatorVersions(namespace).Create(obj)
	if err != nil {
		return nil, errors.WithMessage(err, "installing OperatorVersion")
	}
	return createdObj, nil
}

// InstallInstanceObjToCluster expects a valid Instance obj to install
func (c *Client) InstallInstanceObjToCluster(obj *v1alpha1.Instance, namespace string) (*v1alpha1.Instance, error) {
	createdObj, err := c.clientset.KudoV1alpha1().Instances(namespace).Create(obj)
	if err != nil {
		return nil, errors.WithMessage(err, "installing Instance")
	}
	clog.V(2).Printf("instance %v created in namespace %v", createdObj.Name, namespace)
	return createdObj, nil
}

// DeleteInstance deletes an instance.
func (c *Client) DeleteInstance(instanceName, namespace string) error {
	propagationPolicy := v1.DeletePropagationForeground
	options := &v1.DeleteOptions{
		PropagationPolicy: &propagationPolicy,
	}

	return c.clientset.KudoV1alpha1().Instances(namespace).Delete(instanceName, options)
}

// ValidateServerForOperator validates that the k8s server version and kudo version are valid for operator
// error message will provide detail of failure, otherwise nil
func (c *Client) ValidateServerForOperator(operator *v1alpha1.Operator) error {
	expectedKubver, err := version.New(operator.Spec.KubernetesVersion)
	if err != nil {
		return fmt.Errorf("unable to parse operators kubernetes version: %w", err)
	}
	//TODO : to be added in when we support kudo server providing server version
	//expectedKudoVer, err := semver.NewVersion(operator.Spec.KudoVersion)
	//if err != nil {
	//	return fmt.Errorf("Unable to parse operators kudo version: %w", err)
	//}
	// semvar compares patch, for which we do not want to... compare maj, min only
	kVer, err := getKubeVersion(c.clientset.Discovery())
	if err != nil {
		return err
	}

	kSemVer, err := version.FromGithubVersion(kVer)
	if err != nil {
		return err
	}
	if expectedKubver.CompareMajorMinor(kSemVer) > 0 {
		return fmt.Errorf("expected kubernetes version of %v is not supported with version: %v", expectedKubver, kSemVer)
	}

	return nil
}

// getKubeVersion returns stringified version of k8s server
func getKubeVersion(client discovery.DiscoveryInterface) (string, error) {
	v, err := client.ServerVersion()
	if err != nil {
		return "", err
	}
	return v.String(), nil
}
