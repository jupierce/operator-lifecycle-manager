package e2e

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	extScheme "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/client-go/dynamic"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/component-base/featuregate"
	controllerclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/clientset/versioned"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/registry"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/feature"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/operatorclient"
	pmversioned "github.com/operator-framework/operator-lifecycle-manager/pkg/package-server/client/clientset/versioned"
	"github.com/operator-framework/operator-lifecycle-manager/test/e2e/ctx"
	"github.com/operator-framework/operator-registry/pkg/api/grpc_health_v1"
)

const (
	pollInterval = 1 * time.Second
	pollDuration = 5 * time.Minute

	olmConfigMap = "olm-operators"
	// sync name with scripts/install_local.sh
	packageServerCSV = "packageserver.v1.0.0"
)

var (
	cleaner *namespaceCleaner
	genName = names.SimpleNameGenerator.GenerateName

	persistentCatalogNames               = []string{olmConfigMap}
	nonPersistentCatalogsFieldSelector   = createFieldNotEqualSelector("metadata.name", persistentCatalogNames...)
	persistentConfigMapNames             = []string{olmConfigMap}
	nonPersistentConfigMapsFieldSelector = createFieldNotEqualSelector("metadata.name", persistentConfigMapNames...)
	persistentCSVNames                   = []string{packageServerCSV}
	nonPersistentCSVFieldSelector        = createFieldNotEqualSelector("metadata.name", persistentCSVNames...)
)

type namespaceCleaner struct {
	namespace      string
	skipCleanupOLM bool
}

func newNamespaceCleaner(namespace string) *namespaceCleaner {
	return &namespaceCleaner{
		namespace:      namespace,
		skipCleanupOLM: false,
	}
}

// notifyOnFailure checks if a test has failed or cleanup is true before cleaning a namespace
func (c *namespaceCleaner) NotifyTestComplete(cleanup bool) {
	if CurrentGinkgoTestDescription().Failed {
		c.skipCleanupOLM = true
	}

	if c.skipCleanupOLM || !cleanup {
		ctx.Ctx().Logf("skipping cleanup")
		return
	}

	cleanupOLM(c.namespace)
}

// newKubeClient configures a client to talk to the cluster defined by KUBECONFIG
func newKubeClient() operatorclient.ClientInterface {
	return ctx.Ctx().KubeClient()
}

func newCRClient() versioned.Interface {
	return ctx.Ctx().OperatorClient()
}

func newDynamicClient(t GinkgoTInterface, config *rest.Config) dynamic.Interface {
	return ctx.Ctx().DynamicClient()
}

func newPMClient() pmversioned.Interface {
	return ctx.Ctx().PackageClient()
}

// awaitPods waits for a set of pods to exist in the cluster
func awaitPods(t GinkgoTInterface, c operatorclient.ClientInterface, namespace, selector string, checkPods podsCheckFunc) (*corev1.PodList, error) {
	var fetchedPodList *corev1.PodList
	var err error

	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		fetchedPodList, err = c.KubernetesInterface().CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: selector,
		})

		if err != nil {
			return false, err
		}

		t.Logf("Waiting for pods matching selector %s to match given conditions", selector)

		return checkPods(fetchedPodList), nil
	})

	require.NoError(t, err)
	return fetchedPodList, err
}

func awaitPodsWithInterval(t GinkgoTInterface, c operatorclient.ClientInterface, namespace, selector string, interval time.Duration,
	duration time.Duration, checkPods podsCheckFunc) (*corev1.PodList, error) {
	var fetchedPodList *corev1.PodList
	var err error

	err = wait.Poll(interval, duration, func() (bool, error) {
		fetchedPodList, err = c.KubernetesInterface().CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: selector,
		})

		if err != nil {
			return false, err
		}

		t.Logf("Waiting for pods matching selector %s to match given conditions", selector)

		return checkPods(fetchedPodList), nil
	})

	require.NoError(t, err)
	return fetchedPodList, err
}

// podsCheckFunc describes a function that true if the given PodList meets some criteria; false otherwise.
type podsCheckFunc func(pods *corev1.PodList) bool

// unionPodsCheck returns a podsCheckFunc that represents the union of the given podsCheckFuncs.
func unionPodsCheck(checks ...podsCheckFunc) podsCheckFunc {
	return func(pods *corev1.PodList) bool {
		for _, check := range checks {
			if !check(pods) {
				return false
			}
		}

		return true
	}
}

// podCount returns a podsCheckFunc that returns true if a PodList is of length count; false otherwise.
func podCount(count int) podsCheckFunc {
	return func(pods *corev1.PodList) bool {
		return len(pods.Items) == count
	}
}

// podsReady returns true if all of the pods in the given PodList have a ready condition with ConditionStatus "True"; false otherwise.
func podsReady(pods *corev1.PodList) bool {
	for _, pod := range pods.Items {
		if !podReady(&pod) {
			return false
		}
	}

	return true
}

// podCheckFunc describes a function that returns true if the given Pod meets some criteria; false otherwise.
type podCheckFunc func(pod *corev1.Pod) bool

// hasPodIP returns true if the given Pod has a PodIP.
func hasPodIP(pod *corev1.Pod) bool {
	return pod.Status.PodIP != ""
}

// podReady returns true if the given Pod has a ready condition with ConditionStatus "True"; false otherwise.
func podReady(pod *corev1.Pod) bool {
	var status corev1.ConditionStatus
	for _, condition := range pod.Status.Conditions {
		if condition.Type != corev1.PodReady {
			// Ignore all condition other than PodReady
			continue
		}

		// Found PodReady condition
		status = condition.Status
		break
	}

	return status == corev1.ConditionTrue
}

func awaitPod(t GinkgoTInterface, c operatorclient.ClientInterface, namespace, name string, checkPod podCheckFunc) *corev1.Pod {
	var pod *corev1.Pod
	err := wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		p, err := c.KubernetesInterface().CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		pod = p
		return checkPod(pod), nil
	})
	require.NoError(t, err)

	return pod
}

func awaitAnnotations(t GinkgoTInterface, query func() (metav1.ObjectMeta, error), expected map[string]string) error {
	var err error
	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		t.Logf("Waiting for annotations to match %v", expected)
		obj, err := query()
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		t.Logf("current annotations: %v", obj.GetAnnotations())

		if len(obj.GetAnnotations()) != len(expected) {
			return false, nil
		}

		for key, value := range expected {
			if v, ok := obj.GetAnnotations()[key]; !ok || v != value {
				return false, nil
			}
		}

		t.Logf("Annotations match")
		return true, nil
	})

	return err
}

// compareResources compares resource equality then prints a diff for easier debugging
func compareResources(t GinkgoTInterface, expected, actual interface{}) {
	if eq := equality.Semantic.DeepEqual(expected, actual); !eq {
		t.Fatalf("Resource does not match expected value: %s",
			diff.ObjectDiff(expected, actual))
	}
}

type checkResourceFunc func() error

func waitForDelete(checkResource checkResourceFunc) error {
	var err error
	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		err := checkResource()
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})

	return err
}

func waitForEmptyList(checkList func() (int, error)) error {
	var err error
	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		count, err := checkList()
		if err != nil {
			return false, err
		}
		if count == 0 {
			return true, nil
		}
		return false, nil
	})

	return err
}

func waitForGVR(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, name, namespace string) error {
	return wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		_, err := dynamicClient.Resource(gvr).Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

type catalogSourceCheckFunc func(*v1alpha1.CatalogSource) bool

// This check is disabled for most test runs, but can be enabled for verifying pod health if the e2e tests are running
// in the same kubernetes cluster as the registry pods (currently this only happens with e2e-local-docker)
var checkPodHealth = false

func registryPodHealthy(address string) bool {
	if !checkPodHealth {
		return true
	}

	conn, err := grpc.Dial(address, grpc.WithInsecure())
	if err != nil {
		fmt.Printf("error connecting: %s\n", err.Error())
		return false
	}
	health := grpc_health_v1.NewHealthClient(conn)
	res, err := health.Check(context.TODO(), &grpc_health_v1.HealthCheckRequest{Service: "Registry"})
	if err != nil {
		fmt.Printf("error connecting: %s\n", err.Error())
		return false
	}
	if res.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		fmt.Printf("not healthy: %s\n", res.Status.String())
		return false
	}
	return true
}

func catalogSourceRegistryPodSynced(catalog *v1alpha1.CatalogSource) bool {
	registry := catalog.Status.RegistryServiceStatus
	connState := catalog.Status.GRPCConnectionState
	if registry != nil && connState != nil && !connState.LastConnectTime.IsZero() {
		fmt.Printf("catalog %s pod with address %s\n", catalog.GetName(), registry.Address())
		return registryPodHealthy(registry.Address())
	}
	fmt.Printf("waiting for catalog pod %v to be available (for sync)\n", catalog.GetName())
	return false
}

func fetchCatalogSourceOnStatus(crc versioned.Interface, name, namespace string, check catalogSourceCheckFunc) (*v1alpha1.CatalogSource, error) {
	var fetched *v1alpha1.CatalogSource
	var err error

	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		fetched, err = crc.OperatorsV1alpha1().CatalogSources(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil || fetched == nil {
			fmt.Println(err)
			return false, err
		}
		return check(fetched), nil
	})

	return fetched, err
}

func createFieldNotEqualSelector(field string, names ...string) string {
	var builder strings.Builder
	for i, name := range names {
		builder.WriteString(field)
		builder.WriteString("!=")
		builder.WriteString(name)
		if i < len(names)-1 {
			builder.WriteString(",")
		}
	}

	return builder.String()
}

func cleanupOLM(namespace string) {
	var immediate int64 = 0
	crc := newCRClient()
	c := newKubeClient()

	// Cleanup non persistent OLM CRs
	ctx.Ctx().Logf("cleaning up any remaining non persistent resources...")
	deleteOptions := metav1.DeleteOptions{GracePeriodSeconds: &immediate}
	listOptions := metav1.ListOptions{}

	err := crc.OperatorsV1alpha1().ClusterServiceVersions(namespace).DeleteCollection(context.TODO(), deleteOptions, metav1.ListOptions{FieldSelector: nonPersistentCSVFieldSelector})
	Expect(err).NotTo(HaveOccurred())

	err = crc.OperatorsV1alpha1().InstallPlans(namespace).DeleteCollection(context.TODO(), deleteOptions, listOptions)
	Expect(err).NotTo(HaveOccurred())

	err = crc.OperatorsV1alpha1().Subscriptions(namespace).DeleteCollection(context.TODO(), deleteOptions, listOptions)
	Expect(err).NotTo(HaveOccurred())

	err = crc.OperatorsV1alpha1().CatalogSources(namespace).DeleteCollection(context.TODO(), deleteOptions, metav1.ListOptions{FieldSelector: nonPersistentCatalogsFieldSelector})
	Expect(err).NotTo(HaveOccurred())

	// error: the server does not allow this method on the requested resource
	// Cleanup non persistent configmaps
	err = c.KubernetesInterface().CoreV1().Pods(namespace).DeleteCollection(context.TODO(), deleteOptions, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	err = waitForEmptyList(func() (int, error) {
		res, err := crc.OperatorsV1alpha1().ClusterServiceVersions(namespace).List(context.TODO(), metav1.ListOptions{FieldSelector: nonPersistentCSVFieldSelector})
		ctx.Ctx().Logf("%d %s remaining", len(res.Items), "csvs")
		return len(res.Items), err
	})
	Expect(err).NotTo(HaveOccurred())

	err = waitForEmptyList(func() (int, error) {
		res, err := crc.OperatorsV1alpha1().InstallPlans(namespace).List(context.TODO(), metav1.ListOptions{})
		ctx.Ctx().Logf("%d %s remaining", len(res.Items), "installplans")
		return len(res.Items), err
	})
	Expect(err).NotTo(HaveOccurred())

	err = waitForEmptyList(func() (int, error) {
		res, err := crc.OperatorsV1alpha1().Subscriptions(namespace).List(context.TODO(), metav1.ListOptions{})
		ctx.Ctx().Logf("%d %s remaining", len(res.Items), "subs")
		return len(res.Items), err
	})
	Expect(err).NotTo(HaveOccurred())

	err = waitForEmptyList(func() (int, error) {
		res, err := crc.OperatorsV1alpha1().CatalogSources(namespace).List(context.TODO(), metav1.ListOptions{FieldSelector: nonPersistentCatalogsFieldSelector})
		ctx.Ctx().Logf("%d %s remaining", len(res.Items), "catalogs")
		return len(res.Items), err
	})
	Expect(err).NotTo(HaveOccurred())
}

func buildCatalogSourceCleanupFunc(crc versioned.Interface, namespace string, catalogSource *v1alpha1.CatalogSource) cleanupFunc {
	return func() {
		ctx.Ctx().Logf("Deleting catalog source %s...", catalogSource.GetName())
		err := crc.OperatorsV1alpha1().CatalogSources(namespace).Delete(context.TODO(), catalogSource.GetName(), metav1.DeleteOptions{})
		Expect(err).ToNot(HaveOccurred())
	}
}

func buildConfigMapCleanupFunc(c operatorclient.ClientInterface, namespace string, configMap *corev1.ConfigMap) cleanupFunc {
	return func() {
		ctx.Ctx().Logf("Deleting config map %s...", configMap.GetName())
		err := c.KubernetesInterface().CoreV1().ConfigMaps(namespace).Delete(context.TODO(), configMap.GetName(), metav1.DeleteOptions{})
		Expect(err).ToNot(HaveOccurred())
	}
}

func buildServiceAccountCleanupFunc(t GinkgoTInterface, c operatorclient.ClientInterface, namespace string, serviceAccount *corev1.ServiceAccount) cleanupFunc {
	return func() {
		t.Logf("Deleting service account %s...", serviceAccount.GetName())
		require.NoError(t, c.KubernetesInterface().CoreV1().ServiceAccounts(namespace).Delete(context.TODO(), serviceAccount.GetName(), metav1.DeleteOptions{}))
	}
}

func createInternalCatalogSource(c operatorclient.ClientInterface, crc versioned.Interface, name, namespace string, manifests []registry.PackageManifest, crds []apiextensions.CustomResourceDefinition, csvs []v1alpha1.ClusterServiceVersion) (*v1alpha1.CatalogSource, cleanupFunc) {
	configMap, configMapCleanup := createConfigMapForCatalogData(c, name, namespace, manifests, crds, csvs)

	// Create an internal CatalogSource custom resource pointing to the ConfigMap
	catalogSource := &v1alpha1.CatalogSource{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.CatalogSourceKind,
			APIVersion: v1alpha1.CatalogSourceCRDAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1alpha1.CatalogSourceSpec{
			SourceType: "internal",
			ConfigMap:  configMap.GetName(),
		},
	}
	catalogSource.SetNamespace(namespace)

	ctx.Ctx().Logf("Creating catalog source %s in namespace %s...", name, namespace)
	catalogSource, err := crc.OperatorsV1alpha1().CatalogSources(namespace).Create(context.TODO(), catalogSource, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).ToNot(HaveOccurred())
	}
	ctx.Ctx().Logf("Catalog source %s created", name)

	cleanupInternalCatalogSource := func() {
		configMapCleanup()
		buildCatalogSourceCleanupFunc(crc, namespace, catalogSource)()
	}
	return catalogSource, cleanupInternalCatalogSource
}

func createV1CRDInternalCatalogSource(t GinkgoTInterface, c operatorclient.ClientInterface, crc versioned.Interface, name, namespace string, manifests []registry.PackageManifest, crds []apiextensionsv1.CustomResourceDefinition, csvs []v1alpha1.ClusterServiceVersion) (*v1alpha1.CatalogSource, cleanupFunc) {
	configMap, configMapCleanup := createV1CRDConfigMapForCatalogData(t, c, name, namespace, manifests, crds, csvs)

	// Create an internal CatalogSource custom resource pointing to the ConfigMap
	catalogSource := &v1alpha1.CatalogSource{
		TypeMeta: metav1.TypeMeta{
			Kind:       v1alpha1.CatalogSourceKind,
			APIVersion: v1alpha1.CatalogSourceCRDAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1alpha1.CatalogSourceSpec{
			SourceType: "internal",
			ConfigMap:  configMap.GetName(),
		},
	}
	catalogSource.SetNamespace(namespace)

	ctx.Ctx().Logf("Creating catalog source %s in namespace %s...", name, namespace)
	catalogSource, err := crc.OperatorsV1alpha1().CatalogSources(namespace).Create(context.TODO(), catalogSource, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	ctx.Ctx().Logf("Catalog source %s created", name)

	cleanupInternalCatalogSource := func() {
		configMapCleanup()
		buildCatalogSourceCleanupFunc(crc, namespace, catalogSource)()
	}
	return catalogSource, cleanupInternalCatalogSource
}

func createConfigMapForCatalogData(c operatorclient.ClientInterface, name, namespace string, manifests []registry.PackageManifest, crds []apiextensions.CustomResourceDefinition, csvs []v1alpha1.ClusterServiceVersion) (*corev1.ConfigMap, cleanupFunc) {
	// Create a config map containing the PackageManifests and CSVs
	configMapName := fmt.Sprintf("%s-configmap", name)
	catalogConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: namespace,
		},
		Data: map[string]string{},
	}
	catalogConfigMap.SetNamespace(namespace)

	// Add raw manifests
	if manifests != nil {
		manifestsRaw, err := yaml.Marshal(manifests)
		Expect(err).ToNot(HaveOccurred())
		catalogConfigMap.Data[registry.ConfigMapPackageName] = string(manifestsRaw)
	}

	// Add raw CRDs
	var crdsRaw []byte
	if crds != nil {
		crdStrings := []string{}
		for _, crd := range crds {
			crdStrings = append(crdStrings, serializeCRD(crd))
		}
		var err error
		crdsRaw, err = yaml.Marshal(crdStrings)
		Expect(err).ToNot(HaveOccurred())
	}
	catalogConfigMap.Data[registry.ConfigMapCRDName] = strings.Replace(string(crdsRaw), "- |\n  ", "- ", -1)

	// Add raw CSVs
	if csvs != nil {
		csvsRaw, err := yaml.Marshal(csvs)
		Expect(err).ToNot(HaveOccurred())
		catalogConfigMap.Data[registry.ConfigMapCSVName] = string(csvsRaw)
	}

	createdConfigMap, err := c.KubernetesInterface().CoreV1().ConfigMaps(namespace).Create(context.TODO(), catalogConfigMap, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).ToNot(HaveOccurred())
	}
	return createdConfigMap, buildConfigMapCleanupFunc(c, namespace, createdConfigMap)
}

func createV1CRDConfigMapForCatalogData(t GinkgoTInterface, c operatorclient.ClientInterface, name, namespace string, manifests []registry.PackageManifest, crds []apiextensionsv1.CustomResourceDefinition, csvs []v1alpha1.ClusterServiceVersion) (*corev1.ConfigMap, cleanupFunc) {
	// Create a config map containing the PackageManifests and CSVs
	configMapName := fmt.Sprintf("%s-configmap", name)
	catalogConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: namespace,
		},
		Data: map[string]string{},
	}
	catalogConfigMap.SetNamespace(namespace)

	// Add raw manifests
	if manifests != nil {
		manifestsRaw, err := yaml.Marshal(manifests)
		require.NoError(t, err)
		catalogConfigMap.Data[registry.ConfigMapPackageName] = string(manifestsRaw)
	}

	// Add raw CRDs
	var crdsRaw []byte
	if crds != nil {
		crdStrings := []string{}
		for _, crd := range crds {
			crdStrings = append(crdStrings, serializeV1CRD(t, &crd))
		}
		var err error
		crdsRaw, err = yaml.Marshal(crdStrings)
		require.NoError(t, err)
	}
	catalogConfigMap.Data[registry.ConfigMapCRDName] = strings.Replace(string(crdsRaw), "- |\n  ", "- ", -1)

	// Add raw CSVs
	if csvs != nil {
		csvsRaw, err := yaml.Marshal(csvs)
		require.NoError(t, err)
		catalogConfigMap.Data[registry.ConfigMapCSVName] = string(csvsRaw)
	}

	createdConfigMap, err := c.KubernetesInterface().CoreV1().ConfigMaps(namespace).Create(context.TODO(), catalogConfigMap, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	return createdConfigMap, buildConfigMapCleanupFunc(c, namespace, createdConfigMap)
}

func serializeCRD(crd apiextensions.CustomResourceDefinition) string {
	scheme := runtime.NewScheme()

	Expect(extScheme.AddToScheme(scheme)).Should(Succeed())
	Expect(k8sscheme.AddToScheme(scheme)).Should(Succeed())
	Expect(v1beta1.AddToScheme(scheme)).Should(Succeed())

	out := &v1beta1.CustomResourceDefinition{}
	Expect(scheme.Convert(&crd, out, nil)).To(Succeed())
	out.TypeMeta = metav1.TypeMeta{
		Kind:       "CustomResourceDefinition",
		APIVersion: "apiextensions.k8s.io/v1beta1",
	}

	// set up object serializer
	serializer := k8sjson.NewYAMLSerializer(k8sjson.DefaultMetaFactory, scheme, scheme)

	// create an object manifest
	var manifest bytes.Buffer
	Expect(serializer.Encode(out, &manifest)).To(Succeed())
	return manifest.String()
}

func serializeV1CRD(t GinkgoTInterface, crd *apiextensionsv1.CustomResourceDefinition) string {
	scheme := runtime.NewScheme()
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))

	// set up object serializer
	serializer := k8sjson.NewYAMLSerializer(k8sjson.DefaultMetaFactory, scheme, scheme)

	// create an object manifest
	var manifest bytes.Buffer
	require.NoError(t, serializer.Encode(crd, &manifest))
	return manifest.String()
}

func createCR(c operatorclient.ClientInterface, item *unstructured.Unstructured, apiGroup, version, namespace, resourceKind, resourceName string) (cleanupFunc, error) {
	err := c.CreateCustomResource(item)
	if err != nil {
		return nil, err
	}
	return buildCRCleanupFunc(c, apiGroup, version, namespace, resourceKind, resourceName), nil
}

func buildCRCleanupFunc(c operatorclient.ClientInterface, apiGroup, version, namespace, resourceKind, resourceName string) cleanupFunc {
	return func() {
		err := c.DeleteCustomResource(apiGroup, version, namespace, resourceKind, resourceName)
		if err != nil {
			fmt.Println(err)
		}

		waitForDelete(func() error {
			_, err := c.GetCustomResource(apiGroup, version, namespace, resourceKind, resourceName)
			return err
		})
	}
}

// Local determines whether test is running locally or in a container on openshift-CI.
// Queries for a clusterversion object specific to OpenShift.
func Local(client operatorclient.ClientInterface) (bool, error) {
	const ClusterVersionGroup = "config.openshift.io"
	const ClusterVersionVersion = "v1"
	const ClusterVersionKind = "ClusterVersion"
	gv := metav1.GroupVersion{Group: ClusterVersionGroup, Version: ClusterVersionVersion}.String()

	groups, err := client.KubernetesInterface().Discovery().ServerResourcesForGroupVersion(gv)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return true, fmt.Errorf("checking if cluster is local: checking server groups: %s", err)
	}

	for _, group := range groups.APIResources {
		if group.Kind == ClusterVersionKind {
			return false, nil
		}
	}

	return true, nil
}

// predicateFunc is a predicate for watch events.
type predicateFunc func(event watch.Event) (met bool)

// awaitPredicates waits for all predicates to be met by events of a watch in the order given.
func awaitPredicates(ctx context.Context, w watch.Interface, fns ...predicateFunc) {
	if len(fns) < 1 {
		panic("no predicates given to await")
	}

	i := 0
	for i < len(fns) {
		select {
		case <-ctx.Done():
			Expect(ctx.Err()).ToNot(HaveOccurred())
			return
		case event, ok := <-w.ResultChan():
			if !ok {
				return
			}

			if fns[i](event) {
				i++
			}
		}
	}
}

// filteredPredicate filters events to the given predicate by event type to the given types.
// When no event types are given as arguments, all event types are passed through.
func filteredPredicate(fn predicateFunc, eventTypes ...watch.EventType) predicateFunc {
	return func(event watch.Event) bool {
		valid := true
		for _, eventType := range eventTypes {
			if valid = eventType == event.Type; valid {
				break
			}
		}

		if !valid {
			return false
		}

		return fn(event)
	}
}

func deploymentPredicate(fn func(*appsv1.Deployment) bool) predicateFunc {
	return func(event watch.Event) bool {
		deployment, ok := event.Object.(*appsv1.Deployment)
		Expect(ok).To(BeTrue(), "unexpected event object type %T in deployment", event.Object)

		return fn(deployment)
	}
}

var deploymentAvailable = filteredPredicate(deploymentPredicate(func(deployment *appsv1.Deployment) bool {
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}), watch.Added, watch.Modified)

func deploymentReplicas(replicas int32) predicateFunc {
	return filteredPredicate(deploymentPredicate(func(deployment *appsv1.Deployment) bool {
		return deployment.Status.Replicas == replicas
	}), watch.Added, watch.Modified)
}

const (
	cvoNamespace      = "openshift-cluster-version"
	cvoDeploymentName = "cluster-version-operator"
)

func toggleCVO() {
	c := ctx.Ctx().KubeClient().KubernetesInterface().AppsV1().Deployments(cvoNamespace)
	clientCtx := context.Background()

	Eventually(func() error {
		scale, err := c.GetScale(clientCtx, cvoDeploymentName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// CVO is not enabled
				return nil
			}

			return err
		}

		if scale.Spec.Replicas > 0 {
			scale.Spec.Replicas = 0
		} else {
			scale.Spec.Replicas = 1
		}

		_, err = c.UpdateScale(clientCtx, cvoDeploymentName, scale, metav1.UpdateOptions{})
		return err
	}).Should(Succeed())
}

// togglev2alpha1 toggles the v2alpha1 feature gate on or off.
func togglev2alpha1() {
	// Set the feature flag on OLM's deployment
	c := ctx.Ctx().KubeClient()
	deployment, err := getOperatorDeployment(c, operatorNamespace, labels.Set{"app": "olm-operator"})
	Expect(err).ToNot(HaveOccurred())
	toggleFeatureGates(deployment, feature.OperatorLifecycleManagerV2)
}

// toggleFeatureGates toggles the given feature gates on or off based on their current setting in the deployment.
func toggleFeatureGates(deployment *appsv1.Deployment, toToggle ...featuregate.Feature) {
	var (
		c              = ctx.Ctx().KubeClient().KubernetesInterface().AppsV1().Deployments(deployment.GetNamespace())
		containers     = deployment.Spec.Template.Spec.Containers
		containerIndex = -1
		argIndex       = -1
		prefix         = "--feature-gates="
		gateVals       string
	)

	// Find the container and argument indices for the feature gate option
	for i, container := range containers {
		if container.Name != "olm-operator" {
			continue
		}
		containerIndex = i

		for j, arg := range container.Args {
			if gateVals = strings.TrimPrefix(arg, prefix); arg == gateVals {
				continue
			}
			argIndex = j

			break
		}

		break
	}
	// This should never happen since Deployments must have at least one container
	Expect(containerIndex).ToNot(BeNumerically("<", 0), "deployment %s has no containers", deployment.GetName())

	gate := feature.Gate.DeepCopy()
	if argIndex >= 0 {
		// Collect existing gate values
		Expect(gate.Set(gateVals)).To(Succeed())
	}

	// Toggle gates
	toggled := map[string]bool{}
	for _, feature := range toToggle {
		toggled[string(feature)] = !gate.Enabled(feature)
	}
	Expect(gate.SetFromMap(toggled)).To(Succeed())

	gateArg := fmt.Sprintf("%s%s", prefix, gate)
	if argIndex >= 0 {
		// Overwrite existing gate options
		containers[containerIndex].Args[argIndex] = gateArg
	} else {
		// No existing gate options, add one
		containers[containerIndex].Args = append(containers[containerIndex].Args, gateArg)
	}

	clientCtx := context.Background()
	w, err := c.Watch(clientCtx, metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred())

	Eventually(Apply(deployment, func(d *appsv1.Deployment) error {
		d.Spec = deployment.Spec
		return nil
	})).Should(Succeed())

	deadline, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	awaitPredicates(deadline, w, deploymentReplicas(2), deploymentAvailable, deploymentReplicas(1))
}

type Object interface {
	runtime.Object
	metav1.Object
}

func SetDefaultGroupVersionKind(obj Object) {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Empty() {
		// Best-effort guess the GVK
		gvks, _, err := ctx.Ctx().Scheme().ObjectKinds(obj)
		if err != nil {
			panic(fmt.Sprintf("unable to get gvks for object %T: %s", obj, err))
		}
		if len(gvks) == 0 || gvks[0].Empty() {
			panic(fmt.Sprintf("unexpected gvks registered for object %T: %v", obj, gvks))
		}
		// TODO: The same object can be registered for multiple group versions
		// (although in practise this doesn't seem to be used).
		// In such case, the version set may not be correct.
		gvk = gvks[0]
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk)
}

// Apply returns a function that invokes a change func on an object and performs a server-side apply patch with the result and its status subresource.
// The given resource must be a pointer to a struct that specifies its Name, Namespace, APIVersion, and Kind at minimum.
// The given change function must be unary, matching the signature: "func(<obj type>) error".
// The returned function is suitable for use w/ asyncronous assertions.
// The underlying value of the given resource pointer is updated to reflect the latest cluster state each time the closure is successfully invoked.
// Ex. Change the spec of an existing InstallPlan
//
// plan := &InstallPlan{}
// plan.SetNamespace("ns")
// plan.SetName("install-123def")
// plan.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{
//		Group:   GroupName,
//		Version: GroupVersion,
//		Kind:    InstallPlanKind,
// })
// Eventually(Apply(plan, func(p *v1alpha1.InstallPlan) error {
//		p.Spec.Approved = true
//		return nil
// })).Should(Succeed())
func Apply(obj Object, changeFunc interface{}) func() error {
	// Ensure given object is a pointer
	objType := reflect.TypeOf(obj)
	if objType.Kind() != reflect.Ptr {
		panic(fmt.Sprintf("argument object must be a pointer"))
	}

	// Ensure given function matches expected signature
	var (
		change     = reflect.ValueOf(changeFunc)
		changeType = change.Type()
	)
	if n := changeType.NumIn(); n != 1 {
		panic(fmt.Sprintf("unexpected number of formal parameters in change function signature: expected 1, present %d", n))
	}
	if pt := changeType.In(0); pt.Kind() != reflect.Interface {
		if objType != pt {
			panic(fmt.Sprintf("argument object type does not match the change function parameter type: argument %s, parameter: %s", objType, pt))
		}
	} else if !objType.Implements(pt) {
		panic(fmt.Sprintf("argument object type does not implement the change function parameter type: argument %s, parameter: %s", objType, pt))
	}
	if n := changeType.NumOut(); n != 1 {
		panic(fmt.Sprintf("unexpected number of return values in change function signature: expected 1, present %d", n))
	}
	var err error
	if rt := changeType.Out(0); !rt.Implements(reflect.TypeOf((*error)(nil)).Elem()) {
		panic(fmt.Sprintf("unexpected return type in change function signature: expected %t, present %s", err, rt))
	}

	// Determine if we need to apply a status subresource
	// FIXME: use discovery client instead of relying on storage struct fields.
	_, applyStatus := objType.Elem().FieldByName("Status")

	var (
		bg     = context.Background()
		client = ctx.Ctx().Client()
	)
	key, err := controllerclient.ObjectKeyFromObject(obj)
	if err != nil {
		panic(fmt.Sprintf("unable to extract key from resource: %s", err))
	}

	// Ensure the GVK is set before patching
	SetDefaultGroupVersionKind(obj)

	return func() error {
		changed := func(obj Object) (Object, error) {
			cp := obj.DeepCopyObject().(Object)
			if err := client.Get(bg, key, cp); err != nil {
				return nil, err
			}
			// Reset the GVK after the client call strips it
			SetDefaultGroupVersionKind(cp)
			cp.SetManagedFields(nil)

			out := change.Call([]reflect.Value{reflect.ValueOf(cp)})
			if len(out) != 1 {
				panic(fmt.Sprintf("unexpected number of return values from apply mutation func: expected 1, returned %d", len(out)))
			}

			if err := out[0].Interface(); err != nil {
				return nil, err.(error)
			}

			return cp, nil
		}

		cp, err := changed(obj)
		if err != nil {
			return err
		}

		if err := client.Patch(bg, cp, controllerclient.Apply, controllerclient.ForceOwnership, controllerclient.FieldOwner("test")); err != nil {
			fmt.Printf("first patch error: %s\n", err)
			return err
		}

		if !applyStatus {
			reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(cp).Elem())
			return nil
		}

		cp, err = changed(cp)
		if err != nil {
			return err
		}

		if err := client.Status().Patch(bg, cp, controllerclient.Apply, controllerclient.ForceOwnership, controllerclient.FieldOwner("test")); err != nil {
			fmt.Printf("second patch error: %s\n", err)
			return err
		}

		reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(cp).Elem())

		return nil
	}
}
