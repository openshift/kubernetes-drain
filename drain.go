/*
Copyright 2015 The Kubernetes Authors.

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

package drain

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/go-log/log"

	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

type DrainOptions struct {
	Namespace string

	// Kubernetes client.
	Client kubernetes.Interface

	// Continue even if there are pods not managed by a ReplicationController, ReplicaSet, Job, DaemonSet or StatefulSet.
	Force bool

	// Ignore DaemonSet-managed pods.
	IgnoreDaemonsets bool

	// Period of time in seconds given to each pod to terminate
	// gracefully.  If negative, the default value specified in the pod
	// will be used.
	GracePeriodSeconds int

	// The length of time to wait before giving up on deletion or
	// eviction.  Zero means infinite.
	Timeout time.Duration

	// Continue even if there are pods using emptyDir (local data that
	// will be deleted when the node is drained).
	DeleteLocalData bool

	// Selector (label query) to filter on.
	Selector string

	// Label selector to filter pods on the node.
	PodSelector string

	// Nodes (FIXME: public for Uncordon?)
	nodes []*corev1.Node

	// Logger allows callers to plug in their preferred logger.
	Logger log.Logger
}

// Takes a pod and returns a bool indicating whether or not to operate on the
// pod, an optional warning message, and an optional fatal error.
type podFilter func(corev1.Pod) (include bool, w *warning, f *fatal)
type warning struct {
	string
}
type fatal struct {
	string
}

const (
	EvictionKind        = "Eviction"
	EvictionSubresource = "pods/eviction"

	kDaemonsetFatal      = "DaemonSet-managed pods (use IgnoreDaemonsets to ignore)"
	kDaemonsetWarning    = "ignoring DaemonSet-managed pods"
	kLocalStorageFatal   = "pods with local storage (use DeleteLocalData to override)"
	kLocalStorageWarning = "deleting pods with local storage"
	kUnmanagedFatal      = "pods not managed by ReplicationController, ReplicaSet, Job, DaemonSet or StatefulSet (use Force to override)"
	kUnmanagedWarning    = "deleting pods not managed by ReplicationController, ReplicaSet, Job, DaemonSet or StatefulSet"
)

// GetNodes looks up the nodes (either given by name as arguments or
// by the Selector option).  Matching nodes are cached on
// DrainOptions, so that future cordoning, draining, and uncording can
// act on the same set of nodes without racy lookups.
func (o *DrainOptions) GetNodes(nodes []string) (err error) {
	if len(nodes) == 0 && len(o.Selector) == 0 {
		return nil
	}

	if len(o.Selector) > 0 && len(nodes) > 0 {
		return errors.New("cannot specify both node names and a selector option")
	}

	nodeInterface := o.Client.CoreV1().Nodes()
	o.nodes = []*corev1.Node{}

	for _, node := range nodes {
		node, err := nodeInterface.Get(node, metav1.GetOptions{})
		if err != nil {
			return err
		}
		o.nodes = append(o.nodes, node)
	}

	if len(o.Selector) > 0 {
		nodes, err := nodeInterface.List(metav1.ListOptions{
			LabelSelector: o.Selector,
		})
		if err != nil {
			return err
		}
		for _, node := range nodes.Items {
			o.nodes = append(o.nodes, &node)
		}
	}

	return nil
}

// Drain nodes in preparation for maintenance.
//
// The given nodes will be marked unschedulable to prevent new pods from arriving.
// Drain evicts the pods if the APIServer supports eviction
// (http://kubernetes.io/docs/admin/disruptions/). Otherwise, it will use normal DELETE
// to delete the pods.
// Drain evicts or deletes all pods except mirror pods (which cannot be deleted through
// the API server).  If there are DaemonSet-managed pods, Drain will not proceed
// without IgnoreDaemonsets, and regardless it will not delete any
// DaemonSet-managed pods, because those pods would be immediately replaced by the
// DaemonSet controller, which ignores unschedulable markings.  If there are any
// pods that are neither mirror pods nor managed by ReplicationController,
// ReplicaSet, DaemonSet, StatefulSet or Job, then Drain will not delete any pods unless you
// use Force.  Force will also allow deletion to proceed if the managing resource of one
// or more pods is missing.
//
// Drain waits for graceful termination. You should not operate on the machine until
// the command completes.
//
// When you are ready to put the nodes back into service, use Uncordon, which
// will make the nodes schedulable again.
//
// ![Workflow](http://kubernetes.io/images/docs/kubectl_drain.svg)
func (o *DrainOptions) Drain() (err error) {
	for _, node := range o.nodes {
		if err := o.Cordon(node); err != nil {
			return err
		}
	}

	drainedNodes := sets.NewString()
	var fatal error

	for _, node := range o.nodes {
		err := o.DeleteOrEvictPods(node)
		if err == nil {
			drainedNodes.Insert(node.Name)
			o.logf("drained node %q", node.Name)
		} else {
			o.log(err)
			o.logf("unable to drain node %q", node.Name)
			remainingNodes := []string{}
			fatal = err
			for _, remainingNode := range o.nodes {
				if drainedNodes.Has(remainingNode.Name) {
					continue
				}
				remainingNodes = append(remainingNodes, remainingNode.Name)
			}

			if len(remainingNodes) > 0 {
				sort.Strings(remainingNodes)
				o.logf("there are pending nodes to be drained: %s", strings.Join(remainingNodes, ","))
			}
		}
	}

	return fatal
}

// DeleteOrEvictPods deletes or (where supported) evicts pods from the
// target node and waits until the deletion/eviction completes,
// Timeout elapses, or an error occurs.
func (o *DrainOptions) DeleteOrEvictPods(node *corev1.Node) error {
	pods, err := o.getPodsForDeletion(node)
	if err != nil {
		return err
	}

	err = o.deleteOrEvictPods(pods)
	if err != nil {
		pendingPods, newErr := o.getPodsForDeletion(node)
		if newErr != nil {
			return newErr
		}
		pendingNames := make([]string, len(pendingPods))
		for i, pendingPod := range pendingPods {
			pendingNames[i] = pendingPod.Name
		}
		sort.Strings(pendingNames)
		o.logf("failed to evict pods from node %q (pending pods: %s): %v", node.Name, strings.Join(pendingNames, ","), err)
	}
	return err
}

func (o *DrainOptions) getPodController(pod corev1.Pod) *metav1.OwnerReference {
	return metav1.GetControllerOf(&pod)
}

func (o *DrainOptions) unreplicatedFilter(pod corev1.Pod) (bool, *warning, *fatal) {
	// any finished pod can be removed
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true, nil, nil
	}

	controllerRef := o.getPodController(pod)
	if controllerRef != nil {
		return true, nil, nil
	}
	if o.Force {
		return true, &warning{kUnmanagedWarning}, nil
	}

	return false, nil, &fatal{kUnmanagedFatal}
}

func (o *DrainOptions) daemonsetFilter(pod corev1.Pod) (bool, *warning, *fatal) {
	// Note that we return false in cases where the pod is DaemonSet managed,
	// regardless of flags.  We never delete them, the only question is whether
	// their presence constitutes an error.
	//
	// The exception is for pods that are orphaned (the referencing
	// management resource - including DaemonSet - is not found).
	// Such pods will be deleted if Force is used.
	controllerRef := o.getPodController(pod)
	if controllerRef == nil || controllerRef.Kind != "DaemonSet" {
		return true, nil, nil
	}

	if _, err := o.Client.ExtensionsV1beta1().DaemonSets(pod.Namespace).Get(controllerRef.Name, metav1.GetOptions{}); err != nil {
		// remove orphaned pods with a warning if Force is used
		if apierrors.IsNotFound(err) && o.Force {
			return true, &warning{err.Error()}, nil
		}
		return false, nil, &fatal{err.Error()}
	}

	if !o.IgnoreDaemonsets {
		return false, nil, &fatal{kDaemonsetFatal}
	}

	return false, &warning{kDaemonsetWarning}, nil
}

func mirrorPodFilter(pod corev1.Pod) (bool, *warning, *fatal) {
	if _, found := pod.ObjectMeta.Annotations[corev1.MirrorPodAnnotationKey]; found {
		return false, nil, nil
	}
	return true, nil, nil
}

func hasLocalStorage(pod corev1.Pod) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.EmptyDir != nil {
			return true
		}
	}

	return false
}

func (o *DrainOptions) localStorageFilter(pod corev1.Pod) (bool, *warning, *fatal) {
	if !hasLocalStorage(pod) {
		return true, nil, nil
	}
	if !o.DeleteLocalData {
		return false, nil, &fatal{kLocalStorageFatal}
	}
	return true, &warning{kLocalStorageWarning}, nil
}

// Map of status message to a list of pod names having that status.
type podStatuses map[string][]string

func (ps podStatuses) message() string {
	msgs := []string{}

	for key, pods := range ps {
		msgs = append(msgs, fmt.Sprintf("%s: %s", key, strings.Join(pods, ", ")))
	}
	return strings.Join(msgs, "; ")
}

// getPodsForDeletion receives resource info for a node, and returns all the pods from the given node that we
// are planning on deleting. If there are any pods preventing us from deleting, we return that list in an error.
func (o *DrainOptions) getPodsForDeletion(node *corev1.Node) (pods []corev1.Pod, err error) {
	labelSelector, err := labels.Parse(o.PodSelector)
	if err != nil {
		return pods, err
	}

	podList, err := o.Client.CoreV1().Pods(metav1.NamespaceAll).List(metav1.ListOptions{
		LabelSelector: labelSelector.String(),
		FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": node.Name}).String()})
	if err != nil {
		return pods, err
	}

	ws := podStatuses{}
	fs := podStatuses{}

	for _, pod := range podList.Items {
		podOk := true
		for _, filt := range []podFilter{o.daemonsetFilter, mirrorPodFilter, o.localStorageFilter, o.unreplicatedFilter} {
			filterOk, w, f := filt(pod)

			podOk = podOk && filterOk
			if w != nil {
				ws[w.string] = append(ws[w.string], pod.Name)
			}
			if f != nil {
				fs[f.string] = append(fs[f.string], pod.Name)
			}

			// short-circuit as soon as pod not ok
			// at that point, there is no reason to run pod
			// through any additional filters
			if !podOk {
				break
			}
		}
		if podOk {
			pods = append(pods, pod)
		}
	}

	if len(fs) > 0 {
		return []corev1.Pod{}, errors.New(fs.message())
	}
	if len(ws) > 0 {
		o.log(ws.message())
	}
	return pods, nil
}

func (o *DrainOptions) deletePod(pod corev1.Pod) error {
	deleteOptions := &metav1.DeleteOptions{}
	if o.GracePeriodSeconds >= 0 {
		gracePeriodSeconds := int64(o.GracePeriodSeconds)
		deleteOptions.GracePeriodSeconds = &gracePeriodSeconds
	}
	return o.Client.CoreV1().Pods(pod.Namespace).Delete(pod.Name, deleteOptions)
}

func (o *DrainOptions) evictPod(pod corev1.Pod, policyGroupVersion string) error {
	deleteOptions := &metav1.DeleteOptions{}
	if o.GracePeriodSeconds >= 0 {
		gracePeriodSeconds := int64(o.GracePeriodSeconds)
		deleteOptions.GracePeriodSeconds = &gracePeriodSeconds
	}
	eviction := &policyv1beta1.Eviction{
		TypeMeta: metav1.TypeMeta{
			APIVersion: policyGroupVersion,
			Kind:       EvictionKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		DeleteOptions: deleteOptions,
	}
	// Remember to change change the URL manipulation func when Evction's version change
	return o.Client.PolicyV1beta1().Evictions(eviction.Namespace).Evict(eviction)
}

// deleteOrEvictPods deletes or evicts the pods on the api server
func (o *DrainOptions) deleteOrEvictPods(pods []corev1.Pod) error {
	if len(pods) == 0 {
		return nil
	}

	policyGroupVersion, err := SupportEviction(o.Client)
	if err != nil {
		return err
	}

	getPodFn := func(namespace, name string) (*corev1.Pod, error) {
		return o.Client.CoreV1().Pods(namespace).Get(name, metav1.GetOptions{})
	}

	if len(policyGroupVersion) > 0 {
		return o.evictPods(pods, policyGroupVersion, getPodFn)
	} else {
		return o.deletePods(pods, getPodFn)
	}
}

func (o *DrainOptions) evictPods(pods []corev1.Pod, policyGroupVersion string, getPodFn func(namespace, name string) (*corev1.Pod, error)) error {
	returnCh := make(chan error, 1)

	for _, pod := range pods {
		go func(pod corev1.Pod, returnCh chan error) {
			var err error
			for {
				err = o.evictPod(pod, policyGroupVersion)
				if err == nil {
					break
				} else if apierrors.IsNotFound(err) {
					returnCh <- nil
					return
				} else if apierrors.IsTooManyRequests(err) {
					o.logf("error when evicting pod %q (will retry after 5s): %v", pod.Name, err)
					time.Sleep(5 * time.Second)
				} else {
					returnCh <- fmt.Errorf("error when evicting pod %q: %v", pod.Name, err)
					return
				}
			}
			podArray := []corev1.Pod{pod}
			_, err = o.waitForDelete(podArray, 1*time.Second, time.Duration(math.MaxInt64), true, getPodFn)
			if err == nil {
				returnCh <- nil
			} else {
				returnCh <- fmt.Errorf("error when waiting for pod %q terminating: %v", pod.Name, err)
			}
		}(pod, returnCh)
	}

	doneCount := 0
	var errors []error

	// 0 timeout means infinite, we use MaxInt64 to represent it.
	var globalTimeout time.Duration
	if o.Timeout == 0 {
		globalTimeout = time.Duration(math.MaxInt64)
	} else {
		globalTimeout = o.Timeout
	}
	globalTimeoutCh := time.After(globalTimeout)
	numPods := len(pods)
	for doneCount < numPods {
		select {
		case err := <-returnCh:
			doneCount++
			if err != nil {
				errors = append(errors, err)
			}
		case <-globalTimeoutCh:
			return fmt.Errorf("Drain did not complete within %v", globalTimeout)
		}
	}
	return utilerrors.NewAggregate(errors)
}

func (o *DrainOptions) deletePods(pods []corev1.Pod, getPodFn func(namespace, name string) (*corev1.Pod, error)) error {
	// 0 timeout means infinite, we use MaxInt64 to represent it.
	var globalTimeout time.Duration
	if o.Timeout == 0 {
		globalTimeout = time.Duration(math.MaxInt64)
	} else {
		globalTimeout = o.Timeout
	}
	for _, pod := range pods {
		err := o.deletePod(pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	_, err := o.waitForDelete(pods, 1*time.Second, globalTimeout, false, getPodFn)
	return err
}

func (o *DrainOptions) waitForDelete(pods []corev1.Pod, interval, timeout time.Duration, usingEviction bool, getPodFn func(string, string) (*corev1.Pod, error)) ([]corev1.Pod, error) {
	var verbStr string
	if usingEviction {
		verbStr = "evicted"
	} else {
		verbStr = "deleted"
	}

	err := wait.PollImmediate(interval, timeout, func() (bool, error) {
		pendingPods := []corev1.Pod{}
		for i, pod := range pods {
			p, err := getPodFn(pod.Namespace, pod.Name)
			if apierrors.IsNotFound(err) || (p != nil && p.ObjectMeta.UID != pod.ObjectMeta.UID) {
				o.logf("%s pod %q", verbStr, pod.Name)
				continue
			} else if err != nil {
				return false, err
			} else {
				pendingPods = append(pendingPods, pods[i])
			}
		}
		pods = pendingPods
		if len(pendingPods) > 0 {
			return false, nil
		}
		return true, nil
	})
	return pods, err
}

// SupportEviction uses Discovery API to find out if the server
// supports the eviction subresource.  If supported, it will return
// its groupVersion; otherwise it will return an empty string.
func SupportEviction(clientset kubernetes.Interface) (string, error) {
	discoveryClient := clientset.Discovery()
	groupList, err := discoveryClient.ServerGroups()
	if err != nil {
		return "", err
	}
	foundPolicyGroup := false
	var policyGroupVersion string
	for _, group := range groupList.Groups {
		if group.Name == "policy" {
			foundPolicyGroup = true
			policyGroupVersion = group.PreferredVersion.GroupVersion
			break
		}
	}
	if !foundPolicyGroup {
		return "", nil
	}
	resourceList, err := discoveryClient.ServerResourcesForGroupVersion("v1")
	if err != nil {
		return "", err
	}
	for _, resource := range resourceList.APIResources {
		if resource.Name == EvictionSubresource && resource.Kind == EvictionKind {
			return policyGroupVersion, nil
		}
	}
	return "", nil
}

// Cordon marks a node "Unschedulable".
func (o *DrainOptions) Cordon(node *corev1.Node) error {
	return o.cordonOrUncordon(node, true)
}

// Uncordon marks a node "Schedulable".
func (o *DrainOptions) Uncordon(node *corev1.Node) error {
	return o.cordonOrUncordon(node, false)
}

func (o *DrainOptions) cordonOrUncordon(node *corev1.Node, desired bool) error {
	unsched := node.Spec.Unschedulable
	if unsched == desired {
		return nil
	}

	patch := []byte(fmt.Sprintf("{\"spec\":{\"unschedulable\":%t}}", desired))
	_, err := o.Client.CoreV1().Nodes().Patch(node.Name, types.StrategicMergePatchType, patch)
	if err == nil {
		verbStr := "cordoned"
		if !desired {
			verbStr = "un" + verbStr
		}
		o.logf("%s node %q", verbStr, node.Name)
	}
	return err
}

func (o *DrainOptions) log(v ...interface{}) {
	if o.Logger != nil {
		o.Logger.Log(v...)
	}
}

func (o *DrainOptions) logf(format string, v ...interface{}) {
	if o.Logger != nil {
		o.Logger.Logf(format, v...)
	}
}
