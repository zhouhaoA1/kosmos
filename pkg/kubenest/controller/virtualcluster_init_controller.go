package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kosmos.io/kosmos/pkg/apis/kosmos/v1alpha1"
	"github.com/kosmos.io/kosmos/pkg/kubenest/constants"
	vcnodecontroller "github.com/kosmos.io/kosmos/pkg/kubenest/controller/virtualcluster.node.controller"
	"github.com/kosmos.io/kosmos/pkg/kubenest/util"
)

type VirtualClusterInitController struct {
	client.Client
	Config          *rest.Config
	EventRecorder   record.EventRecorder
	HostPortManager *vcnodecontroller.HostPortManager
	RootClientSet   kubernetes.Interface
	lock            sync.Mutex
}

type NodePool struct {
	Address string            `json:"address" yaml:"address"`
	Labels  map[string]string `json:"labels" yaml:"labels"`
	Cluster string            `json:"cluster" yaml:"cluster"`
	State   string            `json:"state" yaml:"state"`
}

const (
	VirtualClusterControllerFinalizer = "kosmos.io/virtualcluster-controller"
	RequeueTime                       = 10 * time.Second
)

func (c *VirtualClusterInitController) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	startTime := time.Now()
	klog.V(4).InfoS("Started syncing virtual cluster", "virtual cluster", request, "startTime", startTime)
	defer func() {
		klog.V(4).InfoS("Finished syncing virtual cluster", "virtual cluster", request, "duration", time.Since(startTime))
	}()

	originalCluster := &v1alpha1.VirtualCluster{}
	if err := c.Get(ctx, request.NamespacedName, originalCluster); err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(2).InfoS("Virtual Cluster has been deleted", "Virtual Cluster", request)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{RequeueAfter: RequeueTime}, nil
	}
	updatedCluster := originalCluster.DeepCopy()
	updatedCluster.Status.Reason = ""

	//The object is being deleted
	if !originalCluster.DeletionTimestamp.IsZero() {
		err := c.destroyVirtualCluster(updatedCluster)
		if err != nil {
			klog.Errorf("Destroy virtual cluter %s failed. err: %s", updatedCluster.Name, err.Error())
			return reconcile.Result{}, errors.Wrapf(err, "Destroy virtual cluter %s failed. err: %s", updatedCluster.Name, err.Error())
		}
		return c.removeFinalizer(updatedCluster)
	}

	switch originalCluster.Status.Phase {
	case "":
		//create request
		updatedCluster.Status.Phase = v1alpha1.Preparing
		err := c.Update(originalCluster, updatedCluster)
		if err != nil {
			return reconcile.Result{RequeueAfter: RequeueTime}, errors.Wrapf(err, "Error update virtualcluster %s status", updatedCluster.Name)
		}

		err = c.createVirtualCluster(updatedCluster)
		if err != nil {
			klog.Errorf("Failed to create virtualcluster %s. err: %s", updatedCluster.Name, err.Error())
			updatedCluster.Status.Reason = err.Error()
			updatedCluster.Status.Phase = v1alpha1.Pending
			err := c.Update(originalCluster, updatedCluster)
			if err != nil {
				klog.Errorf("Error update virtualcluster %s. err: %s", updatedCluster.Name, err.Error())
				return reconcile.Result{}, errors.Wrapf(err, "Error update virtualcluster %s status", updatedCluster.Name)
			}
			return reconcile.Result{}, errors.Wrap(err, "Error createVirtualCluster")
		}
		updatedCluster.Status.Phase = v1alpha1.Initialized
		err = c.Update(originalCluster, updatedCluster)
		if err != nil {
			klog.Errorf("Error update virtualcluster %s status to %s", updatedCluster.Name, updatedCluster.Status.Phase)
			return reconcile.Result{}, errors.Wrapf(err, "Error update virtualcluster %s status", updatedCluster.Name)
		}
	case v1alpha1.AllNodeReady:
		err := c.ensureAllPodsRunning(updatedCluster, constants.WaitAllPodsRunningTimeoutSeconds*time.Second)
		if err != nil {
			klog.Errorf("Check all pods running err: %s", err.Error())
			updatedCluster.Status.Reason = err.Error()
			updatedCluster.Status.Phase = v1alpha1.Pending
		} else {
			updatedCluster.Status.Phase = v1alpha1.Completed
		}
		err = c.Update(originalCluster, updatedCluster)
		if err != nil {
			klog.Errorf("Error update virtualcluster %s status to %s", updatedCluster.Name, updatedCluster.Status.Phase)
			return reconcile.Result{}, errors.Wrapf(err, "Error update virtualcluster %s status", updatedCluster.Name)
		}
	case v1alpha1.Completed:
		//update request, check if promotepolicy nodes increase or decrease.
		// only 2 scenarios matched update request with status 'completed'.
		// 1. node scale request, original status is 'completed'. 2. node scale process finished by NodeController, the controller changes status from 'updating' to 'completed'
		policyChanged, err := c.checkPromotePoliciesChanged(updatedCluster)
		if err != nil {
			klog.Errorf("Error check promote policies changed. err: %s", err.Error())
			return reconcile.Result{RequeueAfter: RequeueTime}, errors.Wrapf(err, "Error checkPromotePoliciesChanged virtualcluster %s", updatedCluster.Name)
		}
		if !policyChanged {
			return reconcile.Result{}, nil
		} else {
			err := c.assignWorkNodes(updatedCluster)
			if err != nil {
				return reconcile.Result{}, errors.Wrapf(err, "Error update virtualcluster %s", updatedCluster.Name)
			}
			updatedCluster.Status.Phase = v1alpha1.Updating
			err = c.Update(originalCluster, updatedCluster)
			if err != nil {
				klog.Errorf("Error update virtualcluster %s status to %s", updatedCluster.Name, updatedCluster.Status.Phase)
				return reconcile.Result{}, errors.Wrapf(err, "Error update virtualcluster %s status", updatedCluster.Name)
			}
		}
	default:
		klog.Warningf("Skip virtualcluster %s reconcile status: %s", originalCluster.Name, originalCluster.Status.Phase)
	}
	return c.ensureFinalizer(updatedCluster)
}

func (c *VirtualClusterInitController) SetupWithManager(mgr manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(mgr).
		Named(constants.InitControllerName).
		WithOptions(controller.Options{}).
		For(&v1alpha1.VirtualCluster{},
			builder.WithPredicates(predicate.Funcs{
				//	UpdateFunc: c.onVirtualClusterUpdate,
				CreateFunc: func(createEvent event.CreateEvent) bool {
					return true
				},
				UpdateFunc: func(updateEvent event.UpdateEvent) bool { return true },
				DeleteFunc: func(deleteEvent event.DeleteEvent) bool { return true },
			})).
		Complete(c)
}

func (c *VirtualClusterInitController) Update(original, updated *v1alpha1.VirtualCluster) error {
	now := metav1.Now()
	updated.Status.UpdateTime = &now
	return c.Client.Patch(context.TODO(), updated, client.MergeFrom(original))
}

func (c *VirtualClusterInitController) ensureFinalizer(virtualCluster *v1alpha1.VirtualCluster) (reconcile.Result, error) {
	if controllerutil.ContainsFinalizer(virtualCluster, VirtualClusterControllerFinalizer) {
		return reconcile.Result{}, nil
	}

	controllerutil.AddFinalizer(virtualCluster, VirtualClusterControllerFinalizer)
	err := c.Client.Update(context.TODO(), virtualCluster)
	if err != nil {
		return reconcile.Result{Requeue: true}, err
	}

	return reconcile.Result{}, nil
}

func (c *VirtualClusterInitController) removeFinalizer(virtualCluster *v1alpha1.VirtualCluster) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(virtualCluster, VirtualClusterControllerFinalizer) {
		return reconcile.Result{}, nil
	}

	controllerutil.RemoveFinalizer(virtualCluster, VirtualClusterControllerFinalizer)
	err := c.Client.Update(context.TODO(), virtualCluster)
	if err != nil {
		return reconcile.Result{Requeue: true}, err
	}

	return reconcile.Result{}, nil
}

// createVirtualCluster assign work nodes, create control plane and create compoennts from manifests
func (c *VirtualClusterInitController) createVirtualCluster(virtualCluster *v1alpha1.VirtualCluster) error {
	klog.V(2).Infof("Reconciling virtual cluster", "name", virtualCluster.Name)

	executer, err := NewExecutor(virtualCluster, c.Client, c.Config, c.HostPortManager)
	if err != nil {
		return err
	}
	err = c.assignWorkNodes(virtualCluster)
	if err != nil {
		return errors.Wrap(err, "Error in assign work nodes")
	}
	klog.V(2).Infof("Successfully assigned work node for virtual cluster %s", virtualCluster.Name)
	err = executer.Execute()
	if err != nil {
		return err
	}

	secret, err := c.RootClientSet.CoreV1().Secrets(virtualCluster.GetNamespace()).Get(context.TODO(),
		fmt.Sprintf("%s-%s", virtualCluster.GetName(), constants.AdminConfig), metav1.GetOptions{})
	if err != nil {
		return err
	}
	virtualCluster.Spec.Kubeconfig = base64.StdEncoding.EncodeToString(secret.Data[constants.KubeConfig])
	virtualCluster.Status.Phase = v1alpha1.Completed
	return nil
}

func (c *VirtualClusterInitController) destroyVirtualCluster(virtualCluster *v1alpha1.VirtualCluster) error {
	klog.V(2).Infof("Destroying virtual cluster %s", virtualCluster.Name)
	execute, err := NewExecutor(virtualCluster, c.Client, c.Config, c.HostPortManager)
	if err != nil {
		return err
	}
	return execute.Execute()
}

func (c *VirtualClusterInitController) assignWorkNodes(virtualCluster *v1alpha1.VirtualCluster) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	globalNodeList := &v1alpha1.GlobalNodeList{}
	if err := c.Client.List(context.TODO(), globalNodeList); err != nil {
		return fmt.Errorf("list global nodes: %w", err)
	}
	allNodeInfos := make([]v1alpha1.NodeInfo, 0)
	globalNodes := globalNodeList.Items
	for _, policy := range virtualCluster.Spec.PromotePolicies {
		nodeInfos, err := c.assignNodesByPolicy(virtualCluster, policy, globalNodes)
		if err != nil {
			return fmt.Errorf("assign nodes by policy: %w", err)
		}
		allNodeInfos = append(allNodeInfos, nodeInfos...)
	}

	virtualCluster.Spec.PromoteResources.NodeInfos = allNodeInfos
	return nil
}

func (c *VirtualClusterInitController) checkPromotePoliciesChanged(virtualCluster *v1alpha1.VirtualCluster) (bool, error) {
	globalNodeList := &v1alpha1.GlobalNodeList{}
	if err := c.Client.List(context.TODO(), globalNodeList); err != nil {
		return false, fmt.Errorf("list global nodes: %w", err)
	}
	for _, policy := range virtualCluster.Spec.PromotePolicies {
		nodesAssignedMatchedPolicy, err := getAssignedNodesByPolicy(virtualCluster, policy, globalNodeList.Items)
		if err != nil {
			return false, errors.Wrapf(err, "Parse assigned nodes by policy %s error", policy.LabelSelector.String())
		}
		if policy.NodeCount != int32(len(nodesAssignedMatchedPolicy)) {
			klog.V(2).Infof("Promote policy node count changed from %d to %d", len(nodesAssignedMatchedPolicy), policy.NodeCount)
			return true, nil
		}
	}
	return false, nil
}

// nodesChangeCalculate calculate nodes changed when update virtualcluster.
func (c *VirtualClusterInitController) assignNodesByPolicy(virtualCluster *v1alpha1.VirtualCluster, policy v1alpha1.PromotePolicy, globalNodes []v1alpha1.GlobalNode) ([]v1alpha1.NodeInfo, error) {
	nodesAssigned, err := getAssignedNodesByPolicy(virtualCluster, policy, globalNodes)
	if err != nil {
		return nil, errors.Wrapf(err, "Parse assigned nodes by policy %s error", policy.LabelSelector.String())
	}

	requestNodesChanged := policy.NodeCount - int32(len(nodesAssigned))
	if requestNodesChanged == 0 {
		klog.V(2).Infof("Nothing to do for policy %s", policy.LabelSelector.String())
		return nodesAssigned, nil
	} else if requestNodesChanged > 0 { // nodes needs to be increased
		klog.V(2).Infof("Try allocate %d nodes for policy %s", requestNodesChanged, policy.LabelSelector.String())
		var cnt int32 = 0
		var updatedGlobalNodes []*v1alpha1.GlobalNode
		for i, globalNode := range globalNodes {
			if globalNode.Spec.State == v1alpha1.NodeFreeState && mapContains(globalNode.Spec.Labels, policy.LabelSelector.MatchLabels) {
				nodesAssigned = append(nodesAssigned, v1alpha1.NodeInfo{
					NodeName: globalNode.Name,
				})
				// 更新globalNode的状态为占用状态
				updated := globalNode.DeepCopy()
				updated.Spec.State = v1alpha1.NodeInUse
				updated.Status = v1alpha1.GlobalNodeStatus{
					VirtualCluster: virtualCluster.Name,
				}
				globalNodes[i] = *updated
				updatedGlobalNodes = append(updatedGlobalNodes, updated)
				cnt++
			}
			if cnt == requestNodesChanged {
				break
			}
		}
		if cnt < requestNodesChanged {
			return nodesAssigned, errors.Errorf("There is not enough work nodes for promotepolicy %s. Desired %d, matched %d", policy.LabelSelector.String(), requestNodesChanged, cnt)
		}
		for _, updated := range updatedGlobalNodes {
			klog.V(2).Infof("Assign node %s for virtualcluster %s policy %s", updated.Name, virtualCluster.GetName(), policy.LabelSelector.String())
			if err := c.Client.Update(context.TODO(), updated); err != nil {
				return nil, errors.Wrapf(err, "Failed to update globalNode %s to InUse", updated.Name)
			}
		}

	} else { // nodes needs to decrease
		klog.V(2).Infof("Try decrease nodes %d for policy %s", -requestNodesChanged, policy.LabelSelector.String())
		decrease := int(-requestNodesChanged)
		if len(nodesAssigned) < decrease {
			return nil, errors.Errorf("Illegal work nodes decrease operation for promotepolicy %s. Desired %d, matched %d", policy.LabelSelector.String(), decrease, len(nodesAssigned))
		}
		nodesAssigned = nodesAssigned[:len(nodesAssigned)-decrease]
		// note: node pool will not be modified here. NodeController will modify it when node delete success
	}
	return nodesAssigned, nil
}

func getAssignedNodesByPolicy(virtualCluster *v1alpha1.VirtualCluster, policy v1alpha1.PromotePolicy, globalNodes []v1alpha1.GlobalNode) ([]v1alpha1.NodeInfo, error) {
	var nodesAssignedMatchedPolicy []v1alpha1.NodeInfo
	for _, nodeInfo := range virtualCluster.Spec.PromoteResources.NodeInfos {
		node, ok := util.FindGlobalNode(nodeInfo.NodeName, globalNodes)
		if !ok {
			return nil, errors.Errorf("Node %s doesn't find in nodes pool", nodeInfo.NodeName)
		}
		if mapContains(node.Labels, policy.LabelSelector.MatchLabels) {
			nodesAssignedMatchedPolicy = append(nodesAssignedMatchedPolicy, nodeInfo)
		}
	}
	return nodesAssignedMatchedPolicy, nil
}

func (c *VirtualClusterInitController) ensureAllPodsRunning(virtualCluster *v1alpha1.VirtualCluster, timeout time.Duration) error {
	secret, err := c.RootClientSet.CoreV1().Secrets(virtualCluster.GetNamespace()).Get(context.TODO(),
		fmt.Sprintf("%s-%s", virtualCluster.GetName(), constants.AdminConfig), metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "Get virtualcluster kubeconfig secret error")
	}
	config, err := clientcmd.RESTConfigFromKubeConfig(secret.Data[constants.KubeConfig])
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	namespaceList, err := clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(err, "List namespaces error")
	}
	endTime := time.Now().Second() + int(timeout.Seconds())
	for _, namespace := range namespaceList.Items {
		startTime := time.Now().Second()
		if startTime > endTime {
			return errors.New("Timeout waiting for all pods running")
		}
		klog.V(2).Infof("Check if all pods ready in namespace %s", namespace.Name)
		err := wait.PollWithContext(context.TODO(), 5*time.Second, time.Duration(endTime-startTime)*time.Second, func(ctx context.Context) (done bool, err error) {
			klog.V(2).Infof("Check if all deployments ready in namespace %s", namespace.Name)
			deployList, err := clientset.AppsV1().Deployments(namespace.Name).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, errors.Wrapf(err, "Get deployment list in namespace %s error", namespace.Name)
			}
			for _, deploy := range deployList.Items {
				if deploy.Status.AvailableReplicas != deploy.Status.Replicas {
					klog.V(2).Infof("Deployment %s/%s is not ready yet. Available replicas: %d, Desired: %d. Waiting...", deploy.Name, namespace.Name, deploy.Status.AvailableReplicas, deploy.Status.Replicas)
					return false, nil
				}
			}

			klog.V(2).Infof("Check if all statefulset ready in namespace %s", namespace.Name)
			stsList, err := clientset.AppsV1().StatefulSets(namespace.Name).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, errors.Wrapf(err, "Get statefulset list in namespace %s error", namespace.Name)
			}
			for _, sts := range stsList.Items {
				if sts.Status.AvailableReplicas != sts.Status.Replicas {
					klog.V(2).Infof("Statefulset %s/%s is not ready yet. Available replicas: %d, Desired: %d. Waiting...", sts.Name, namespace.Name, sts.Status.AvailableReplicas, sts.Status.Replicas)
					return false, nil
				}
			}

			klog.V(2).Infof("Check if all daemonset ready in namespace %s", namespace.Name)
			damonsetList, err := clientset.AppsV1().DaemonSets(namespace.Name).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, errors.Wrapf(err, "Get daemonset list in namespace %s error", namespace.Name)
			}
			for _, daemonset := range damonsetList.Items {
				if daemonset.Status.CurrentNumberScheduled != daemonset.Status.NumberReady {
					klog.V(2).Infof("Daemonset %s/%s is not ready yet. Scheduled replicas: %d, Ready: %d. Waiting...", daemonset.Name, namespace.Name, daemonset.Status.CurrentNumberScheduled, daemonset.Status.NumberReady)
					return false, nil
				}
			}

			return true, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func mapContains(big map[string]string, small map[string]string) bool {
	for k, v := range small {
		if bigV, ok := big[k]; !ok || bigV != v {
			return false
		}
	}
	return true
}
